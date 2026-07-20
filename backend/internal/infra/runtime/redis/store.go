package redis

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/repository"
	redisclient "github.com/redis/go-redis/v9"
)

const (
	concurrencyLeaseGrace       = time.Minute
	maxStickyBindingsPerAccount = 10000
	maxDeviceSessions           = 1000
	maxQuotaRecoveryEvents      = 100000
)

var rateScript = redisclient.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
if current > tonumber(ARGV[1]) then return 0 end
return 1
`)

var observeRoutePerformanceScript = redisclient.NewScript(`
local now = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local alpha = tonumber(ARGV[3])
local success_sample = tonumber(ARGV[4])
local latency = tonumber(ARGV[5])
local circuit_failure = tonumber(ARGV[6])
local circuit_threshold = tonumber(ARGV[7])
local circuit_window = tonumber(ARGV[8])
local circuit_open = tonumber(ARGV[9])
local updated = tonumber(redis.call('HGET', KEYS[1], 'updated_at') or '0')
if updated == 0 or now - updated > ttl then redis.call('DEL', KEYS[1]) end
local samples = tonumber(redis.call('HGET', KEYS[1], 'samples') or '0')
local success = tonumber(redis.call('HGET', KEYS[1], 'success_ewma') or '0')
local latency_ewma = tonumber(redis.call('HGET', KEYS[1], 'latency_ewma_ms') or '0')
if samples == 0 then
  success = success_sample
  latency_ewma = latency
else
  success = alpha * success_sample + (1 - alpha) * success
  if latency > 0 then
    if latency_ewma <= 0 then latency_ewma = latency
    else latency_ewma = alpha * latency + (1 - alpha) * latency_ewma end
  end
end
samples = samples + 1
local consecutive = tonumber(redis.call('HGET', KEYS[1], 'consecutive_failures') or '0')
local last_failure = tonumber(redis.call('HGET', KEYS[1], 'last_circuit_failure_at') or '0')
local circuit_until = tonumber(redis.call('HGET', KEYS[1], 'circuit_open_until') or '0')
if success_sample == 1 then
  consecutive = 0
  last_failure = 0
  circuit_until = 0
elseif circuit_failure == 1 then
  if last_failure == 0 or now - last_failure > circuit_window then consecutive = 0 end
  consecutive = consecutive + 1
  last_failure = now
  if consecutive >= circuit_threshold then
    circuit_until = now + circuit_open
    consecutive = 0
  end
end
redis.call('HSET', KEYS[1],
  'success_ewma', success, 'latency_ewma_ms', latency_ewma, 'samples', samples,
  'consecutive_failures', consecutive, 'last_circuit_failure_at', last_failure,
  'circuit_open_until', circuit_until, 'updated_at', now)
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

var acquireLeaseScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1
`)

var releaseLeaseScript = redisclient.NewScript(`return redis.call('ZREM', KEYS[1], ARGV[1])`)

var releaseLockScript = redisclient.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

var setStickyScript = redisclient.NewScript(`
local old = redis.call('GET', KEYS[1])
if old and old ~= ARGV[1] then redis.call('ZREM', ARGV[3] .. old, KEYS[1]) end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[4])
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
local excess = redis.call('ZCARD', KEYS[2]) - tonumber(ARGV[6])
if excess > 0 then
  local stale = redis.call('ZRANGE', KEYS[2], 0, excess - 1)
  for _, key in ipairs(stale) do
    if redis.call('GET', key) == ARGV[1] then redis.call('DEL', key) end
    redis.call('ZREM', KEYS[2], key)
  end
end
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
return 1
`)

var deleteStickyByAccountScript = redisclient.NewScript(`
local members = redis.call('ZRANGE', KEYS[1], 0, -1)
local deleted = 0
for _, key in ipairs(members) do
  if redis.call('GET', key) == ARGV[1] then
    deleted = deleted + redis.call('DEL', key)
  end
end
redis.call('DEL', KEYS[1])
return deleted
`)

var createDeviceSessionScript = redisclient.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[2])
if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[4]) then return 0 end
if not redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3], 'NX') then return -1 end
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[3]) then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
return 1
`)

var updateDeviceSessionScript = redisclient.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2], 'XX')
redis.call('ZADD', KEYS[2], ARGV[3], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[2], ARGV[2]) end
return 1
`)

var deleteDeviceSessionScript = redisclient.NewScript(`
redis.call('ZREM', KEYS[2], KEYS[1])
return redis.call('DEL', KEYS[1])
`)

var scheduleQuotaRecoveryScript = redisclient.NewScript(`
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

var ensureQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 2 end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[4]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return 1
`)

var claimQuotaRecoveryScript = redisclient.NewScript(`
local values = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
local result = {}
for _, value in ipairs(values) do
  redis.call('ZADD', KEYS[1], ARGV[3], value)
  redis.call('HSET', KEYS[3], value, ARGV[4])
  table.insert(result, value)
  table.insert(result, redis.call('HGET', KEYS[2], value) or '0')
  table.insert(result, ARGV[4])
end
return result
`)

var ackQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
return redis.call('ZREM', KEYS[1], ARGV[1])
`)

var rescheduleQuotaRecoveryScript = redisclient.NewScript(`
if redis.call('HGET', KEYS[3], ARGV[1]) ~= ARGV[4] then return 0 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('HDEL', KEYS[3], ARGV[1])
return 1
`)

// Config 表示 Redis 运行态存储的启动配置。
type Config struct {
	Address          string
	Username         string
	Password         string
	Database         int
	KeyPrefix        string
	TLS              bool
	ConcurrencyLease time.Duration
}

// Store 实现多实例共享的限流、并发租约、粘滞路由、Device OAuth 会话和分布式锁。
type Store struct {
	client           *redisclient.Client
	prefix           string
	concurrencyLease time.Duration
}

// Open 连接 Redis；选中的 Redis 不可用时直接返回启动错误。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	options := &redisclient.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.Database}
	if cfg.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := redisclient.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连接 Redis: %w", err)
	}
	lease := cfg.ConcurrencyLease
	if lease <= 0 {
		lease = 3 * time.Hour
	}
	return &Store{client: client, prefix: cfg.KeyPrefix, concurrencyLease: lease}, nil
}

func (s *Store) Close() error { return s.client.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func (s *Store) key(namespace, key string) string { return s.prefix + namespace + ":" + key }

// PublishSettingsChanged 发布运行设置失效通知，不在 Redis 中复制设置内容。
func (s *Store) PublishSettingsChanged(ctx context.Context) error {
	return s.client.Publish(ctx, s.key("events", "settings"), "reload").Err()
}

// ListenSettingsChanges 监听设置变更并调用重载函数，go-redis 会在连接中断后自动重连。
func (s *Store) ListenSettingsChanges(ctx context.Context, handler func(context.Context) error) error {
	pubsub := s.client.Subscribe(ctx, s.key("events", "settings"))
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-channel:
			if !ok {
				return errors.New("Redis 设置通知通道已关闭")
			}
			if err := handler(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *Store) Allow(ctx context.Context, key string, limit int, _ time.Time) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	result, err := rateScript.Run(ctx, s.client, []string{s.key("rate", key)}, limit, time.Minute.Milliseconds()).Int()
	return result == 1, err
}

func (s *Store) acquireConcurrency(ctx context.Context, key string, limit int) (func(), bool, error) {
	if limit <= 0 {
		return func() {}, true, nil
	}
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(s.concurrencyLease)
	redisKey := s.key("concurrency", key)
	result, err := acquireLeaseScript.Run(ctx, s.client, []string{redisKey}, now.UnixMilli(), limit, expiresAt.UnixMilli(), token, (s.concurrencyLease + concurrencyLeaseGrace).Milliseconds()).Int()
	if err != nil || result != 1 {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaseLeaseScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err()
		})
	}, true, nil
}

func (s *Store) Current(ctx context.Context, key string) (int, error) {
	redisKey := s.key("concurrency", key)
	now := time.Now().UTC().UnixMilli()
	pipe := s.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, redisKey, "-inf", strconv.FormatInt(now, 10))
	count := pipe.ZCard(ctx, redisKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(count.Val()), nil
}

func (s *Store) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	values := make(map[string]int, len(keys))
	if len(keys) == 0 {
		return values, nil
	}
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	pipe := s.client.Pipeline()
	counts := make(map[string]*redisclient.IntCmd, len(keys))
	for _, key := range keys {
		redisKey := s.key("concurrency", key)
		pipe.ZRemRangeByScore(ctx, redisKey, "-inf", now)
		counts[key] = pipe.ZCard(ctx, redisKey)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for key, count := range counts {
		values[key] = int(count.Val())
	}
	return values, nil
}

func (s *Store) Get(ctx context.Context, key string, now time.Time) (uint64, bool, error) {
	value, err := s.client.Get(ctx, s.key("sticky", key)).Result()
	if errors.Is(err, redisclient.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err := strconv.ParseUint(value, 10, 64)
	return id, err == nil, err
}

func (s *Store) Set(ctx context.Context, key string, accountID uint64, expiresAt time.Time) error {
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return nil
	}
	id := strconv.FormatUint(accountID, 10)
	bindingKey := s.key("sticky", key)
	accountSetPrefix := s.prefix + "sticky-account:"
	accountSetKey := accountSetPrefix + id
	now := time.Now().UTC()
	return setStickyScript.Run(ctx, s.client, []string{bindingKey, accountSetKey}, id, ttl.Milliseconds(), accountSetPrefix, now.UnixMilli(), expiresAt.UnixMilli(), maxStickyBindingsPerAccount).Err()
}

func (s *Store) DeleteByAccount(ctx context.Context, accountID uint64) error {
	id := strconv.FormatUint(accountID, 10)
	return deleteStickyByAccountScript.Run(ctx, s.client, []string{s.key("sticky-account", id)}, id).Err()
}

func (s *Store) ObserveRoutePerformance(ctx context.Context, observation repository.RoutePerformanceObservation, policy repository.RoutePerformancePolicy) error {
	key, ok := redisRoutePerformanceKey(observation.Key)
	if !ok {
		return repository.ErrInvalid
	}
	policy = normalizeRoutePerformancePolicy(policy)
	now := observation.ObservedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	success, circuitFailure := 0, 0
	if observation.Success {
		success = 1
	}
	if observation.CircuitFailure {
		circuitFailure = 1
	}
	return observeRoutePerformanceScript.Run(ctx, s.client, []string{s.key("route-performance", key)},
		now.UnixMilli(), policy.TTL.Milliseconds(), policy.Alpha, success, max(int64(0), observation.Latency.Milliseconds()),
		circuitFailure, policy.CircuitThreshold, policy.CircuitWindow.Milliseconds(), policy.CircuitOpenDuration.Milliseconds()).Err()
}

func (s *Store) GetRoutePerformances(ctx context.Context, keys []repository.RoutePerformanceKey, now time.Time) (map[repository.RoutePerformanceKey]repository.RoutePerformance, error) {
	result := make(map[repository.RoutePerformanceKey]repository.RoutePerformance, len(keys))
	type pendingRoutePerformance struct {
		key repository.RoutePerformanceKey
		cmd *redisclient.MapStringStringCmd
	}
	pending := make([]pendingRoutePerformance, 0, len(keys))
	pipe := s.client.Pipeline()
	for _, key := range keys {
		redisKey, ok := redisRoutePerformanceKey(key)
		if !ok {
			continue
		}
		key.UpstreamModel = strings.TrimSpace(key.UpstreamModel)
		pending = append(pending, pendingRoutePerformance{key: key, cmd: pipe.HGetAll(ctx, s.key("route-performance", redisKey))})
	}
	if len(pending) == 0 {
		return result, nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for _, item := range pending {
		fields := item.cmd.Val()
		if len(fields) == 0 {
			continue
		}
		value, ok := parseRoutePerformance(fields)
		if !ok || (!value.UpdatedAt.IsZero() && now.Sub(value.UpdatedAt) > routePerformanceDefaultTTL) {
			continue
		}
		if value.CircuitOpenUntil != nil && !now.Before(*value.CircuitOpenUntil) {
			value.CircuitOpenUntil = nil
		}
		result[item.key] = value
	}
	return result, nil
}

const routePerformanceDefaultTTL = 30 * time.Minute

func redisRoutePerformanceKey(value repository.RoutePerformanceKey) (string, bool) {
	model := strings.TrimSpace(value.UpstreamModel)
	if value.AccountID == 0 || model == "" || len(model) > 255 {
		return "", false
	}
	digest := sha256.Sum256([]byte(model))
	return strconv.FormatUint(value.AccountID, 10) + ":" + fmt.Sprintf("%x", digest[:12]), true
}

func normalizeRoutePerformancePolicy(value repository.RoutePerformancePolicy) repository.RoutePerformancePolicy {
	if value.Alpha <= 0 || value.Alpha > 1 {
		value.Alpha = .25
	}
	if value.TTL <= 0 {
		value.TTL = routePerformanceDefaultTTL
	}
	if value.CircuitThreshold <= 0 {
		value.CircuitThreshold = 3
	}
	if value.CircuitWindow <= 0 {
		value.CircuitWindow = 2 * time.Minute
	}
	if value.CircuitOpenDuration <= 0 {
		value.CircuitOpenDuration = 2 * time.Minute
	}
	return value
}

func parseRoutePerformance(fields map[string]string) (repository.RoutePerformance, bool) {
	parseFloat := func(name string) (float64, bool) {
		value, err := strconv.ParseFloat(fields[name], 64)
		return value, err == nil
	}
	parseInt := func(name string) (int64, bool) {
		value, err := strconv.ParseInt(fields[name], 10, 64)
		return value, err == nil
	}
	success, successOK := parseFloat("success_ewma")
	latencyMS, latencyOK := parseFloat("latency_ewma_ms")
	samples, samplesOK := parseInt("samples")
	updatedMS, updatedOK := parseInt("updated_at")
	if !successOK || !latencyOK || !samplesOK || !updatedOK || samples < 0 {
		return repository.RoutePerformance{}, false
	}
	value := repository.RoutePerformance{
		SuccessEWMA: success, LatencyEWMA: time.Duration(latencyMS * float64(time.Millisecond)), Samples: samples,
		UpdatedAt: time.UnixMilli(updatedMS).UTC(),
	}
	if consecutive, ok := parseInt("consecutive_failures"); ok && consecutive > 0 {
		value.ConsecutiveFailures = int(consecutive)
	}
	if lastFailureMS, ok := parseInt("last_circuit_failure_at"); ok && lastFailureMS > 0 {
		lastFailure := time.UnixMilli(lastFailureMS).UTC()
		value.LastCircuitFailureAt = &lastFailure
	}
	if circuitUntilMS, ok := parseInt("circuit_open_until"); ok && circuitUntilMS > 0 {
		circuitUntil := time.UnixMilli(circuitUntilMS).UTC()
		value.CircuitOpenUntil = &circuitUntil
	}
	return value, true
}

func (s *Store) ScheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := scheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) EnsureQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	if value.AccountID == 0 || value.Mode == "" || value.DueAt.IsZero() {
		return fmt.Errorf("额度恢复事件无效")
	}
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ensureQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), maxQuotaRecoveryEvents).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("额度恢复队列已满")
	}
	return nil
}

func (s *Store) ClaimDueQuotaRecoveries(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]account.QuotaRecoveryEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	claimToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	values, err := claimQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, now.UnixMilli(), limit, now.Add(lease).UnixMilli(), claimToken).StringSlice()
	if err != nil {
		return nil, err
	}
	result := make([]account.QuotaRecoveryEvent, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		raw := values[index]
		idText, mode, ok := strings.Cut(raw, ":")
		id, parseErr := strconv.ParseUint(idText, 10, 64)
		attempts, attemptsErr := strconv.Atoi(values[index+1])
		if ok && parseErr == nil && id > 0 && mode != "" {
			if attemptsErr != nil || attempts < 0 {
				attempts = 0
			}
			result = append(result, account.QuotaRecoveryEvent{AccountID: id, Mode: mode, DueAt: now, Attempts: attempts, ClaimToken: values[index+2]})
		}
	}
	return result, nil
}

func (s *Store) AckQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := ackQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) RescheduleQuotaRecovery(ctx context.Context, value account.QuotaRecoveryEvent) error {
	member := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
	result, err := rescheduleQuotaRecoveryScript.Run(ctx, s.client, []string{s.key("quota-recovery", "events"), s.key("quota-recovery", "attempts"), s.key("quota-recovery", "claims")}, member, value.DueAt.UnixMilli(), max(0, value.Attempts), value.ClaimToken).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) Create(ctx context.Context, value account.DeviceSession) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ttl := time.Until(value.ExpiresAt)
	if ttl <= 0 {
		return repository.ErrNotFound
	}
	now := time.Now().UTC()
	result, err := createDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", value.ID), s.key("device-index", "sessions")}, payload, now.UnixMilli(), ttl.Milliseconds(), maxDeviceSessions, value.ExpiresAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return repository.ErrConflict
	}
	return nil
}

func (s *Store) GetDevice(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	payload, err := s.client.Get(ctx, s.key("device", id)).Bytes()
	if errors.Is(err, redisclient.Nil) {
		return account.DeviceSession{}, repository.ErrNotFound
	}
	if err != nil {
		return account.DeviceSession{}, err
	}
	var value account.DeviceSession
	if err := json.Unmarshal(payload, &value); err != nil {
		return account.DeviceSession{}, err
	}
	if !now.Before(value.ExpiresAt) {
		_ = deleteDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}).Err()
		return account.DeviceSession{}, repository.ErrNotFound
	}
	return value, nil
}

func (s *Store) Update(ctx context.Context, value account.DeviceSession) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ttl := time.Until(value.ExpiresAt)
	if ttl <= 0 {
		return repository.ErrNotFound
	}
	result, err := updateDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", value.ID), s.key("device-index", "sessions")}, payload, ttl.Milliseconds(), value.ExpiresAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return repository.ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	return deleteDeviceSessionScript.Run(ctx, s.client, []string{s.key("device", id), s.key("device-index", "sessions")}).Err()
}

func (s *Store) acquireLock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	redisKey := s.key("lock", key)
	acquired, err := s.client.SetNX(ctx, redisKey, token, ttl).Result()
	if err != nil || !acquired {
		return nil, acquired, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = releaseLockScript.Run(releaseCtx, s.client, []string{redisKey}, token).Err()
		})
	}, true, nil
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// DeviceSessionStore 适配 DeviceSessionRepository，避免与 StickySessionRepository 的 Get 签名冲突。
type DeviceSessionStore struct{ store *Store }

func NewDeviceSessionStore(store *Store) *DeviceSessionStore {
	return &DeviceSessionStore{store: store}
}
func (s *DeviceSessionStore) Create(ctx context.Context, value account.DeviceSession) error {
	return s.store.Create(ctx, value)
}
func (s *DeviceSessionStore) Get(ctx context.Context, id string, now time.Time) (account.DeviceSession, error) {
	return s.store.GetDevice(ctx, id, now)
}
func (s *DeviceSessionStore) Update(ctx context.Context, value account.DeviceSession) error {
	return s.store.Update(ctx, value)
}
func (s *DeviceSessionStore) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
}

// ConcurrencyLimiter 适配 ConcurrencyLimiter，避免与 DistributedLock 的 Acquire 签名冲突。
type ConcurrencyLimiter struct{ store *Store }

func NewConcurrencyLimiter(store *Store) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{store: store}
}
func (l *ConcurrencyLimiter) Acquire(ctx context.Context, key string, limit int) (func(), bool, error) {
	return l.store.acquireConcurrency(ctx, key, limit)
}
func (l *ConcurrencyLimiter) Current(ctx context.Context, key string) (int, error) {
	return l.store.Current(ctx, key)
}
func (l *ConcurrencyLimiter) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	return l.store.CurrentMany(ctx, keys)
}

// LockStore 适配 DistributedLock。
type LockStore struct{ store *Store }

func NewLockStore(store *Store) *LockStore { return &LockStore{store: store} }
func (l *LockStore) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	return l.store.acquireLock(ctx, strings.TrimSpace(key), ttl)
}

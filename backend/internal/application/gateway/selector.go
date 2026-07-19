package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/observability"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

type accountLease struct {
	Credential     account.Credential
	Billing        *account.Billing
	QuotaProbe     bool
	QuotaProbeKind account.QuotaRecoveryKind
	QuotaMode      string
	release        func()
}

const quotaProbeLease = 5 * time.Minute
const successPersistInterval = 30 * time.Second
const candidateCacheTTL = time.Second
const routePerformanceTTL = 30 * time.Minute

const (
	routePerformanceAlpha   = 0.25
	routePerformancePrior   = 0.90
	routePerformanceWarmup  = 8
	routePerformanceMaxSize = 4096
)

type candidateSnapshot struct {
	values    []account.RoutingCandidate
	expiresAt time.Time
}

type candidateCacheKey struct {
	provider      account.Provider
	upstreamModel string
	quotaMode     string
}

type routePerformanceKey struct {
	accountID     uint64
	upstreamModel string
}

type routePerformance struct {
	successEWMA float64
	latencyEWMA time.Duration
	samples     int
	updatedAt   time.Time
}

type SelectionUnavailableReason string

const (
	SelectionNoAccounts       SelectionUnavailableReason = "no_accounts"
	SelectionUnsupportedModel SelectionUnavailableReason = "unsupported_model"
	SelectionCooling          SelectionUnavailableReason = "cooling"
	SelectionModelCooling     SelectionUnavailableReason = "model_cooling"
	SelectionQuotaExhausted   SelectionUnavailableReason = "quota_exhausted"
	SelectionSaturated        SelectionUnavailableReason = "saturated"
)

// SelectionUnavailableError 保留选号失败的真实原因，避免所有情况都退化成模糊的 503。
type SelectionUnavailableError struct {
	Reason     SelectionUnavailableReason
	RetryAfter time.Duration
}

type routingBlockKind uint8

const (
	routingBlockNone routingBlockKind = iota
	routingBlockModelCooling
	routingBlockCooling
	routingBlockQuotaRecovery
	routingBlockQuota
)

type routingBlock struct {
	kind    routingBlockKind
	retryAt time.Time
}

type candidateState uint8

const (
	candidateEligible candidateState = iota
	candidateDisabled
	candidateReauthRequired
	candidateUnsupported
	candidateModelCooling
	candidateCooling
	candidateQuotaExhausted
)

type candidateEvaluation struct {
	state   candidateState
	retryAt time.Time
}

// candidateRoutingBlock is the single inference eligibility policy. Recovery
// states are deliberately checked before billing/window snapshots so a stale
// positive window cannot re-enable an exhausted account.
func candidateRoutingBlock(candidate account.RoutingCandidate, value account.Credential, now time.Time) routingBlock {
	if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
		return routingBlock{kind: routingBlockModelCooling, retryAt: candidate.ModelQuotaBlock.CooldownUntil}
	}
	if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
		return routingBlock{kind: routingBlockCooling, retryAt: *value.CooldownUntil}
	}
	if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
		var retryAt time.Time
		if recovery.NextProbeAt != nil {
			retryAt = *recovery.NextProbeAt
		}
		return routingBlock{kind: routingBlockQuotaRecovery, retryAt: retryAt}
	}
	if candidate.FreeQuota && candidate.ObservedTokens >= account.EstimatedFreeTokenLimit {
		return routingBlock{kind: routingBlockQuota}
	}
	if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
		return routingBlock{kind: routingBlockQuota}
	}
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Remaining <= 0 {
		var retryAt time.Time
		if candidate.QuotaWindow.ResetAt != nil {
			retryAt = *candidate.QuotaWindow.ResetAt
		}
		return routingBlock{kind: routingBlockQuota, retryAt: retryAt}
	}
	return routingBlock{}
}

// evaluateCandidate is the single account eligibility classifier used by both
// request routing and the read-only operations snapshot.
func evaluateCandidate(candidate account.RoutingCandidate, now time.Time) candidateEvaluation {
	value := candidate.Credential
	if !value.Enabled {
		return candidateEvaluation{state: candidateDisabled}
	}
	if value.AuthStatus != account.AuthStatusActive {
		return candidateEvaluation{state: candidateReauthRequired}
	}
	if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
		return candidateEvaluation{state: candidateUnsupported}
	}
	block := candidateRoutingBlock(candidate, value, now)
	switch block.kind {
	case routingBlockModelCooling:
		return candidateEvaluation{state: candidateModelCooling, retryAt: block.retryAt}
	case routingBlockCooling:
		return candidateEvaluation{state: candidateCooling, retryAt: block.retryAt}
	case routingBlockQuotaRecovery, routingBlockQuota:
		return candidateEvaluation{state: candidateQuotaExhausted, retryAt: block.retryAt}
	default:
		return candidateEvaluation{state: candidateEligible}
	}
}

func (e *SelectionUnavailableError) Error() string {
	if e == nil {
		return "没有可用上游账号"
	}
	switch e.Reason {
	case SelectionUnsupportedModel:
		return "当前账号池不支持该模型"
	case SelectionCooling:
		return "可用上游账号正在冷却"
	case SelectionModelCooling:
		return "可用上游账号的目标模型正在冷却"
	case SelectionQuotaExhausted:
		return "可用上游账号额度等待恢复"
	case SelectionSaturated:
		return "可用上游账号均达到并发上限"
	default:
		return "没有可用上游账号"
	}
}

func (l *accountLease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// Selector 实现可替换的 balanced 账号选择策略。
type Selector struct {
	accounts          repository.AccountRepository
	concurrency       repository.ConcurrencyLimiter
	sticky            repository.StickySessionRepository
	stickyTTL         time.Duration
	cooldownBase      time.Duration
	cooldownMax       time.Duration
	capacityWait      time.Duration
	mu                sync.Mutex
	leaseWakeMu       sync.Mutex
	leaseWake         chan struct{}
	lastSelectedAt    map[uint64]time.Time
	lastSuccessAt     map[uint64]time.Time
	candidates        map[candidateCacheKey]candidateSnapshot
	roundRobinLast    map[candidateCacheKey]uint64
	performance       map[routePerformanceKey]routePerformance
	sharedPerformance repository.RoutePerformanceRepository
	logger            *slog.Logger
	candidateLoads    singleflight.Group
	tierOrders        interface {
		TierOrder(account.Provider, string) []account.WebTier
	}
}

func NewSelector(accounts repository.AccountRepository, concurrency repository.ConcurrencyLimiter, sticky repository.StickySessionRepository, tierOrders interface {
	TierOrder(account.Provider, string) []account.WebTier
}, stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) *Selector {
	wait := time.Duration(0)
	if len(capacityWait) > 0 && capacityWait[0] > 0 {
		wait = capacityWait[0]
	}
	return &Selector{accounts: accounts, concurrency: concurrency, sticky: sticky, tierOrders: tierOrders, stickyTTL: stickyTTL, cooldownBase: cooldownBase, cooldownMax: cooldownMax, capacityWait: wait, leaseWake: make(chan struct{}), lastSelectedAt: make(map[uint64]time.Time), lastSuccessAt: make(map[uint64]time.Time), candidates: make(map[candidateCacheKey]candidateSnapshot), roundRobinLast: make(map[candidateCacheKey]uint64), performance: make(map[routePerformanceKey]routePerformance), logger: slog.Default()}
}

func (s *Selector) SetRoutePerformanceRepository(value repository.RoutePerformanceRepository) {
	s.mu.Lock()
	s.sharedPerformance = value
	s.mu.Unlock()
}

func (s *Selector) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	s.mu.Lock()
	s.logger = logger
	s.mu.Unlock()
}

func (s *Selector) UpdateConfig(stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) {
	s.mu.Lock()
	s.stickyTTL = stickyTTL
	s.cooldownBase = cooldownBase
	s.cooldownMax = cooldownMax
	if len(capacityWait) > 0 {
		s.capacityWait = max(time.Duration(0), capacityWait[0])
	}
	s.mu.Unlock()
}

func (s *Selector) routingConfig() (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stickyTTL, s.cooldownBase, s.cooldownMax, s.capacityWait
}

func (s *Selector) Acquire(ctx context.Context, provider account.Provider, upstreamModel, quotaMode, promptCacheKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	now := time.Now().UTC()
	stickyKey := promptCacheStickyKey(promptCacheKey)
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	normalCandidates := make([]account.RoutingCandidate, 0, len(values))
	probeCandidates := make([]account.RoutingCandidate, 0, len(values))
	supportedCandidates := 0
	consideredCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time
	circuitStates := s.routeCircuitStates(ctx, values, upstreamModel, now)
	for _, candidate := range values {
		value := candidate.Credential
		if excluded[value.ID] {
			continue
		}
		evaluation := evaluateCandidate(candidate, now)
		if evaluation.state == candidateDisabled || evaluation.state == candidateReauthRequired {
			continue
		}
		consideredCandidates++
		if evaluation.state == candidateUnsupported {
			continue
		}
		supportedCandidates++
		if circuitUntil := circuitStates[value.ID]; !circuitUntil.IsZero() && now.Before(circuitUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, circuitUntil, now)
			continue
		}
		switch evaluation.state {
		case candidateModelCooling:
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, evaluation.retryAt, now)
			continue
		case candidateCooling:
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, evaluation.retryAt, now)
			continue
		case candidateQuotaExhausted:
			if allowQuotaProbe && candidate.QuotaRecovery != nil && candidate.QuotaRecovery.NextProbeAt != nil && !now.Before(*candidate.QuotaRecovery.NextProbeAt) {
				probeCandidates = append(probeCandidates, candidate)
			} else {
				quotaCandidates++
				earliestRetry = earlierFuture(earliestRetry, evaluation.retryAt, now)
			}
			continue
		}
		normalCandidates = append(normalCandidates, candidate)
	}
	recordRoutingPool(ctx, RoutingTraceEvent{
		Total: len(values), Excluded: len(excluded), Eligible: len(normalCandidates), Probe: len(probeCandidates),
		Cooling: coolingCandidates, ModelCooling: modelCoolingCandidates, QuotaExhausted: quotaCandidates,
		Unsupported: max(0, consideredCandidates-supportedCandidates),
	})
	if len(normalCandidates) == 0 && len(probeCandidates) == 0 {
		reason := SelectionNoAccounts
		switch {
		case consideredCandidates > 0 && supportedCandidates == 0:
			reason = SelectionUnsupportedModel
		case modelCoolingCandidates > 0:
			reason = SelectionModelCooling
		case coolingCandidates > 0:
			reason = SelectionCooling
		case quotaCandidates > 0:
			reason = SelectionQuotaExhausted
		}
		recordRoutingFailure(ctx, reason)
		return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry)}
	}
	if len(probeCandidates) > 0 {
		if err := s.sortCandidates(ctx, probeCandidates, now, s.resolveTierOrder(provider, upstreamModel), upstreamModel); err != nil {
			return nil, err
		}
		for _, candidate := range probeCandidates {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				continue
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = candidate.QuotaRecovery.Kind
			lease.Billing = candidate.Billing
			recordRoutingSelection(ctx, candidate.Credential.ID, "quota_probe")
			return lease, nil
		}
	}
	if len(normalCandidates) > 0 {
		shadow := append([]account.RoutingCandidate(nil), normalCandidates...)
		if err := s.sortCandidates(ctx, shadow, now, s.resolveTierOrder(provider, upstreamModel), upstreamModel); err == nil && len(shadow) > 0 {
			recordRoutingShadowSelection(ctx, shadow[0].Credential.ID)
		}
	}
	if stickyKey != "" {
		stickyID, ok, err := s.sticky.Get(ctx, stickyKey, now)
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok {
			for _, candidate := range normalCandidates {
				if candidate.Credential.ID == stickyID {
					lease, acquireErr := s.claimAccountSlot(ctx, candidate.Credential)
					if acquireErr != nil {
						return nil, acquireErr
					}
					if lease != nil {
						lease.Billing = candidate.Billing
						lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
						recordRoutingSelection(ctx, candidate.Credential.ID, "sticky")
						return lease, nil
					}
				}
			}
		}
	}
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	roundRobinKey := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
	reservedAccountID := s.orderRoundRobinCandidates(normalCandidates, roundRobinKey, s.resolveTierOrder(provider, upstreamModel))
	for {
		currentTime := time.Now().UTC()
		for _, candidate := range normalCandidates {
			lease, err := s.claimAccountSlot(ctx, candidate.Credential)
			if err != nil {
				return nil, err
			}
			if lease == nil {
				continue
			}
			if stickyKey != "" {
				stickyTTL, _, _, _ := s.routingConfig()
				if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, currentTime.Add(stickyTTL)); err != nil {
					lease.Release()
					return nil, fmt.Errorf("写入会话粘滞状态: %w", err)
				}
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
			s.commitRoundRobinSelection(roundRobinKey, reservedAccountID, candidate.Credential.ID)
			recordRoutingSelection(ctx, candidate.Credential.ID, "balanced")
			return lease, nil
		}
		if capacityWait <= 0 {
			recordRoutingFailure(ctx, SelectionSaturated)
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			recordRoutingFailure(ctx, SelectionSaturated)
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

// CapacitySnapshot evaluates capacity with the same blocking policy used by Acquire, without reserving an account.
func (s *Selector) CapacitySnapshot(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, recoveryLead time.Duration) (account.RoutingCapacity, error) {
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return account.RoutingCapacity{}, err
	}
	result := account.RoutingCapacity{Total: len(values)}
	eligible := make([]account.RoutingCandidate, 0, len(values))
	circuitStates := s.routeCircuitStates(ctx, values, upstreamModel, now)
	for _, candidate := range values {
		evaluation := evaluateCandidate(candidate, now)
		if evaluation.state == candidateEligible {
			if circuitUntil := circuitStates[candidate.Credential.ID]; !circuitUntil.IsZero() && now.Before(circuitUntil) {
				result.ModelCooling++
				if result.EarliestRecovery == nil || circuitUntil.Before(*result.EarliestRecovery) {
					value := circuitUntil
					result.EarliestRecovery = &value
				}
				continue
			}
		}
		switch evaluation.state {
		case candidateDisabled:
			result.Disabled++
		case candidateReauthRequired:
			result.ReauthRequired++
		case candidateUnsupported:
			result.Unsupported++
		case candidateModelCooling:
			result.ModelCooling++
		case candidateCooling:
			result.Cooling++
		case candidateQuotaExhausted:
			result.QuotaExhausted++
			if evaluation.retryAt.After(now) {
				if recoveryLead >= 0 && !evaluation.retryAt.After(now.Add(recoveryLead)) {
					result.RecoveringSoon++
				}
				if result.EarliestRecovery == nil || evaluation.retryAt.Before(*result.EarliestRecovery) {
					value := evaluation.retryAt
					result.EarliestRecovery = &value
				}
			}
		case candidateEligible:
			eligible = append(eligible, candidate)
		}
	}
	current, err := s.currentConcurrency(ctx, eligible)
	if err != nil {
		return account.RoutingCapacity{}, err
	}
	for _, candidate := range eligible {
		value := candidate.Credential
		inFlight := max(0, current[value.ID])
		result.InFlight += inFlight
		if value.MaxConcurrent <= 0 {
			result.Unlimited++
			result.Eligible++
			continue
		}
		result.TotalSlots += value.MaxConcurrent
		available := max(0, value.MaxConcurrent-inFlight)
		result.AvailableSlots += available
		if available == 0 {
			result.Saturated++
		} else {
			result.Eligible++
		}
	}
	observability.ObserveRouteCapacity(string(provider), upstreamModel, map[string]int{
		"total": result.Total, "eligible": result.Eligible, "saturated": result.Saturated,
		"disabled": result.Disabled, "reauth_required": result.ReauthRequired, "quota_exhausted": result.QuotaExhausted,
		"recovering_soon": result.RecoveringSoon, "cooling": result.Cooling, "model_cooling": result.ModelCooling,
		"unsupported": result.Unsupported, "in_flight": result.InFlight, "total_slots": result.TotalSlots,
		"available_slots": result.AvailableSlots,
	})
	return result, nil
}

func (s *Selector) currentConcurrency(ctx context.Context, values []account.RoutingCandidate) (map[uint64]int, error) {
	result := make(map[uint64]int, len(values))
	keys := make([]string, 0, len(values))
	for _, candidate := range values {
		keys = append(keys, fmt.Sprintf("account:%d", candidate.Credential.ID))
	}
	if batchReader, ok := s.concurrency.(repository.ConcurrencySnapshotReader); ok {
		snapshot, err := batchReader.CurrentMany(ctx, keys)
		if err != nil {
			return nil, fmt.Errorf("批量读取账号并发租约: %w", err)
		}
		for _, candidate := range values {
			result[candidate.Credential.ID] = snapshot[fmt.Sprintf("account:%d", candidate.Credential.ID)]
		}
		return result, nil
	}
	for _, candidate := range values {
		current, err := s.concurrency.Current(ctx, fmt.Sprintf("account:%d", candidate.Credential.ID))
		if err != nil {
			return nil, fmt.Errorf("读取账号并发租约: %w", err)
		}
		result[candidate.Credential.ID] = current
	}
	return result, nil
}

// orderRoundRobinCandidates gives each healthy route pool an independent turn.
// Capability and tier groups retain their routing precedence; accounts within a
// group use stable ID order so retries and cache reloads remain predictable.
func (s *Selector) orderRoundRobinCandidates(values []account.RoutingCandidate, key candidateCacheKey, tierOrder []account.WebTier) uint64 {
	if len(values) == 0 {
		return 0
	}
	sort.SliceStable(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.SupportsModel != right.SupportsModel {
			return left.SupportsModel
		}
		if left.ModelCapabilityKnown != right.ModelCapabilityKnown {
			return left.ModelCapabilityKnown
		}
		leftTier := tierOrderRank(tierOrder, left.Credential.WebTier)
		rightTier := tierOrderRank(tierOrder, right.Credential.WebTier)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		return values[i].Credential.ID < values[j].Credential.ID
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roundRobinLast == nil {
		s.roundRobinLast = make(map[candidateCacheKey]uint64)
	}
	lastAccountID := s.roundRobinLast[key]
	for start := 0; start < len(values); {
		end := start + 1
		for end < len(values) && sameRoundRobinGroup(values[start], values[end], tierOrder) {
			end++
		}
		rotateCandidateGroupAfter(values[start:end], lastAccountID)
		start = end
	}
	reservedAccountID := values[0].Credential.ID
	s.roundRobinLast[key] = reservedAccountID
	return reservedAccountID
}

func sameRoundRobinGroup(left, right account.RoutingCandidate, tierOrder []account.WebTier) bool {
	return left.SupportsModel == right.SupportsModel &&
		left.ModelCapabilityKnown == right.ModelCapabilityKnown &&
		tierOrderRank(tierOrder, left.Credential.WebTier) == tierOrderRank(tierOrder, right.Credential.WebTier)
}

func rotateCandidateGroupAfter(values []account.RoutingCandidate, lastAccountID uint64) {
	if len(values) < 2 || lastAccountID == 0 {
		return
	}
	offset := 0
	for index, candidate := range values {
		if candidate.Credential.ID > lastAccountID {
			offset = index
			break
		}
	}
	if offset == 0 {
		return
	}
	ordered := append([]account.RoutingCandidate(nil), values[offset:]...)
	ordered = append(ordered, values[:offset]...)
	copy(values, ordered)
}

func (s *Selector) commitRoundRobinSelection(key candidateCacheKey, reservedAccountID, selectedAccountID uint64) {
	if selectedAccountID == 0 || selectedAccountID == reservedAccountID {
		return
	}
	s.mu.Lock()
	if s.roundRobinLast[key] == reservedAccountID {
		s.roundRobinLast[key] = selectedAccountID
	}
	s.mu.Unlock()
}

// promptCacheStickyKey 将调用方缓存键压缩为固定长度，仅用于本地账号粘滞索引。
func promptCacheStickyKey(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// AcquirePinned 为 previous_response_id 等账号归属请求获取指定账号租约。
func (s *Selector) AcquirePinned(ctx context.Context, provider account.Provider, accountID uint64, upstreamModel, quotaMode string, inference bool) (*accountLease, error) {
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, upstreamModel, quotaMode, now)
	if err != nil {
		return nil, err
	}
	circuitStates := s.routeCircuitStates(ctx, values, upstreamModel, now)
	for _, candidate := range values {
		value := candidate.Credential
		if value.ID != accountID {
			continue
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if inference {
			if circuitUntil := circuitStates[value.ID]; !circuitUntil.IsZero() && now.Before(circuitUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, circuitUntil)}
			}
			if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
				return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
			}
			block := candidateRoutingBlock(candidate, value, now)
			switch block.kind {
			case routingBlockModelCooling:
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, block.retryAt)}
			case routingBlockCooling:
				return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, block.retryAt)}
			case routingBlockQuotaRecovery, routingBlockQuota:
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryDelay(now, block.retryAt)}
			}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value)
		if err != nil {
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = effectiveQuotaMode(candidate, quotaMode)
		recordRoutingSelection(ctx, value.ID, "pinned")
		return lease, nil
	}
	recordRoutingFailure(ctx, SelectionNoAccounts)
	return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
}

func effectiveQuotaMode(candidate account.RoutingCandidate, fallback string) string {
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Mode == "weekly" {
		return "weekly"
	}
	return fallback
}

func (s *Selector) MarkSuccess(ctx context.Context, credential account.Credential) {
	s.markSuccess(ctx, credential, true)
}

// ApplyInspectionHealthy persists the health reset used by an automatic
// inspection action. Unlike the routing fast path, persistence errors are
// returned so the inspection result is not falsely marked as applied.
func (s *Selector) ApplyInspectionHealthy(ctx context.Context, credential account.Credential) error {
	if err := s.accounts.UpdateHealth(ctx, credential.ID, 0, nil, "", true); err != nil {
		return err
	}
	if err := s.accounts.ClearQuotaRecovery(ctx, credential.ID); err != nil {
		return err
	}
	s.invalidateCandidates(credential.Provider)
	return nil
}

func (s *Selector) markSuccess(ctx context.Context, credential account.Credential, quotaProbe bool) {
	now := time.Now().UTC()
	persist := credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != ""
	s.mu.Lock()
	if last := s.lastSuccessAt[credential.ID]; last.IsZero() || now.Sub(last) >= successPersistInterval {
		persist = true
	}
	if persist {
		s.lastSuccessAt[credential.ID] = now
	}
	s.mu.Unlock()
	if persist {
		_ = s.accounts.UpdateHealth(ctx, credential.ID, 0, nil, "", true)
	}
	if quotaProbe {
		_ = s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
	if quotaProbe || credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != "" {
		s.invalidateCandidates(credential.Provider)
	}
}

func (s *Selector) MarkFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64) {
	if err := s.ApplyFreeQuotaExhausted(ctx, credential, used, limit); err != nil {
		s.logger.Warn("quota_recovery_state_write_failed", "account_id", credential.ID, "kind", account.QuotaRecoveryKindFree, "error", err)
	}
}

func (s *Selector) ApplyFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64) error {
	now := time.Now().UTC()
	nextProbeAt := now.Add(24 * time.Hour)
	if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &now,
		NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	if s.sticky != nil {
		if err := s.sticky.DeleteByAccount(ctx, credential.ID); err != nil {
			return err
		}
	}
	s.invalidateCandidates(credential.Provider)
	return nil
}

func (s *Selector) MarkModelQuotaExhausted(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) {
	if err := s.ApplyModelQuotaExhausted(ctx, credential, upstreamModel, retryAfter); err != nil {
		s.logger.Warn("model_quota_state_write_failed", "account_id", credential.ID, "model", upstreamModel, "error", err)
	}
}

func (s *Selector) ApplyModelQuotaExhausted(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) error {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return s.ApplyFreeQuotaExhausted(ctx, credential, 0, 0)
	}
	if retryAfter <= 0 {
		retryAfter = 24 * time.Hour
	}
	until := time.Now().UTC().Add(retryAfter)
	if err := s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_quota_depleted", CooldownUntil: until, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	s.invalidateCandidates(credential.Provider)
	return nil
}

// MarkPaidQuotaExhausted 使用已知真实账期将付费账号移出号池，到期后才允许 Billing 探测。
func (s *Selector) MarkPaidQuotaExhausted(ctx context.Context, credential account.Credential, billing *account.Billing) bool {
	if err := s.ApplyPaidQuotaExhausted(ctx, credential, billing); err != nil {
		s.logger.Warn("paid_quota_state_write_failed", "account_id", credential.ID, "error", err)
		return false
	}
	return true
}

func (s *Selector) ApplyPaidQuotaExhausted(ctx context.Context, credential account.Credential, billing *account.Billing) error {
	now := time.Now().UTC()
	periodEnd := now.Add(24 * time.Hour)
	if billing != nil {
		if parsed, ok := billing.PeriodEnd(); ok && parsed.After(now) {
			periodEnd = parsed
		}
	}
	if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &periodEnd, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	if s.sticky != nil {
		if err := s.sticky.DeleteByAccount(ctx, credential.ID); err != nil {
			return err
		}
	}
	s.invalidateCandidates(credential.Provider)
	return nil
}

// MarkQuotaStateChanged 在 Billing 探测改变持久化额度状态后立即失效候选快照。
func (s *Selector) MarkQuotaStateChanged(provider account.Provider) { s.invalidateCandidates(provider) }

// ObserveRouteResult keeps a short-lived per-account/model signal for adaptive selection.
func (s *Selector) ObserveRouteResult(accountID uint64, upstreamModel string, latency time.Duration, success bool) {
	s.observeRouteResult(accountID, upstreamModel, latency, success, false)
}

func (s *Selector) ObserveRouteFailure(accountID uint64, upstreamModel string, latency time.Duration, decision FailureDecision) {
	if decision.PenalizeAccount {
		observability.ObserveCircuitFailure(strings.TrimSpace(upstreamModel))
	}
	s.observeRouteResult(accountID, upstreamModel, latency, false, decision.PenalizeAccount)
}

func (s *Selector) observeRouteResult(accountID uint64, upstreamModel string, latency time.Duration, success, circuitFailure bool) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if accountID == 0 || upstreamModel == "" {
		return
	}
	if latency < 0 {
		latency = 0
	}
	now := time.Now().UTC()
	key := routePerformanceKey{accountID: accountID, upstreamModel: upstreamModel}
	s.mu.Lock()
	if s.performance == nil {
		s.performance = make(map[routePerformanceKey]routePerformance)
	}
	value, exists := s.performance[key]
	if !exists && len(s.performance) >= routePerformanceMaxSize {
		var oldestKey routePerformanceKey
		var oldestAt time.Time
		for candidateKey, candidate := range s.performance {
			if now.Sub(candidate.updatedAt) > routePerformanceTTL {
				delete(s.performance, candidateKey)
				continue
			}
			if oldestAt.IsZero() || candidate.updatedAt.Before(oldestAt) {
				oldestKey, oldestAt = candidateKey, candidate.updatedAt
			}
		}
		if len(s.performance) >= routePerformanceMaxSize && !oldestAt.IsZero() {
			delete(s.performance, oldestKey)
		}
	}
	sample := 0.0
	if success {
		sample = 1
	}
	if !exists || now.Sub(value.updatedAt) > routePerformanceTTL {
		value = routePerformance{successEWMA: sample, latencyEWMA: latency, samples: 1}
	} else {
		value.successEWMA = routePerformanceAlpha*sample + (1-routePerformanceAlpha)*value.successEWMA
		if value.latencyEWMA <= 0 {
			value.latencyEWMA = latency
		} else if latency > 0 {
			value.latencyEWMA = time.Duration(routePerformanceAlpha*float64(latency) + (1-routePerformanceAlpha)*float64(value.latencyEWMA))
		}
		value.samples++
	}
	value.updatedAt = now
	s.performance[key] = value
	shared := s.sharedPerformance
	logger := s.logger
	s.mu.Unlock()
	if shared != nil {
		observeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := shared.ObserveRoutePerformance(observeCtx, repository.RoutePerformanceObservation{
			Key: repository.RoutePerformanceKey{AccountID: accountID, UpstreamModel: upstreamModel}, Latency: latency,
			Success: success, CircuitFailure: circuitFailure, ObservedAt: now,
		}, routePerformancePolicy())
		cancel()
		if err != nil && logger != nil {
			logger.Warn("route_performance_observe_failed", "account_id", accountID, "model", upstreamModel, "error", err)
		}
	}
}

func routePerformancePolicy() repository.RoutePerformancePolicy {
	return repository.RoutePerformancePolicy{
		Alpha: routePerformanceAlpha, TTL: routePerformanceTTL, CircuitThreshold: 3,
		CircuitWindow: 2 * time.Minute, CircuitOpenDuration: 2 * time.Minute,
	}
}

func (s *Selector) sharedRoutePerformances(ctx context.Context, values []account.RoutingCandidate, upstreamModel string, now time.Time) map[uint64]repository.RoutePerformance {
	upstreamModel = strings.TrimSpace(upstreamModel)
	s.mu.Lock()
	shared, logger := s.sharedPerformance, s.logger
	s.mu.Unlock()
	if shared == nil || upstreamModel == "" || len(values) == 0 {
		return nil
	}
	keys := make([]repository.RoutePerformanceKey, 0, len(values))
	for _, candidate := range values {
		keys = append(keys, repository.RoutePerformanceKey{AccountID: candidate.Credential.ID, UpstreamModel: upstreamModel})
	}
	loaded, err := shared.GetRoutePerformances(ctx, keys, now)
	if err != nil {
		if logger != nil {
			logger.Warn("route_performance_read_failed", "model", upstreamModel, "error", err)
		}
		return nil
	}
	result := make(map[uint64]repository.RoutePerformance, len(loaded))
	for key, value := range loaded {
		result[key.AccountID] = value
	}
	return result
}

func (s *Selector) routeCircuitStates(ctx context.Context, values []account.RoutingCandidate, upstreamModel string, now time.Time) map[uint64]time.Time {
	performance := s.sharedRoutePerformances(ctx, values, upstreamModel, now)
	result := make(map[uint64]time.Time)
	for accountID, value := range performance {
		if value.CircuitOpenUntil != nil && now.Before(*value.CircuitOpenUntil) {
			result[accountID] = value.CircuitOpenUntil.UTC()
		}
	}
	return result
}

// ConsumeQuota 将成功请求的本地额度变化应用到候选快照，避免为单账号变化清空整个 Provider 缓存。
func (s *Selector) ConsumeQuota(provider account.Provider, accountID uint64, mode string, amount int) {
	if accountID == 0 || mode == "" || mode == "weekly" || amount <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, snapshot := range s.candidates {
		if key.provider != provider {
			continue
		}
		for index := range snapshot.values {
			candidate := &snapshot.values[index]
			if candidate.Credential.ID != accountID || candidate.QuotaWindow == nil || candidate.QuotaWindow.Mode != mode {
				continue
			}
			window := *candidate.QuotaWindow
			window.Remaining = max(0, window.Remaining-amount)
			window.UpdatedAt = time.Now().UTC()
			candidate.QuotaWindow = &window
		}
		s.candidates[key] = snapshot
	}
}

func (s *Selector) MarkFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) {
	failureCount := credential.FailureCount + 1
	_, cooldownBase, cooldownMax, _ := s.routingConfig()
	cooldown := cooldownBase
	for i := 1; i < failureCount && cooldown < cooldownMax; i++ {
		cooldown *= 2
	}
	if cooldown > cooldownMax {
		cooldown = cooldownMax
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
	}
	until := time.Now().UTC().Add(cooldown)
	_ = s.accounts.UpdateHealth(ctx, credential.ID, failureCount, &until, fmt.Sprintf("upstream status %d", status), false)
	s.invalidateCandidates(credential.Provider)
	if status == 401 || status == 402 || status == 403 || status == 429 {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
}

func (s *Selector) loadCandidates(ctx context.Context, provider account.Provider, upstreamModel, quotaMode string, now time.Time) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, upstreamModel: upstreamModel, quotaMode: quotaMode}
	s.mu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		values := append([]account.RoutingCandidate(nil), snapshot.values...)
		s.mu.Unlock()
		return values, nil
	}
	s.mu.Unlock()
	loadKey := string(provider) + "\x00" + upstreamModel + "\x00" + quotaMode
	loaded, err, _ := s.candidateLoads.Do(loadKey, func() (any, error) {
		checkTime := time.Now().UTC()
		s.mu.Lock()
		if snapshot, ok := s.candidates[key]; ok && checkTime.Before(snapshot.expiresAt) {
			values := append([]account.RoutingCandidate(nil), snapshot.values...)
			s.mu.Unlock()
			return values, nil
		}
		s.mu.Unlock()
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.candidates[key] = candidateSnapshot{values: append([]account.RoutingCandidate(nil), values...), expiresAt: checkTime.Add(candidateCacheTTL)}
		s.mu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]account.RoutingCandidate(nil), loaded.([]account.RoutingCandidate)...), nil
}

func (s *Selector) invalidateCandidates(provider account.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.candidates {
		if key.provider == provider {
			delete(s.candidates, key)
		}
	}
}

func (s *Selector) claimAccountSlot(ctx context.Context, value account.Credential) (*accountLease, error) {
	limit := value.MaxConcurrent
	if limit <= 0 {
		limit = account.DefaultMaxConcurrent
	}
	release, acquired, err := s.concurrency.Acquire(ctx, fmt.Sprintf("account:%d", value.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("获取账号并发租约: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	s.mu.Lock()
	s.lastSelectedAt[value.ID] = time.Now().UTC()
	s.mu.Unlock()
	return &accountLease{Credential: value, release: func() {
		release()
		s.announceLeaseReturn()
	}}, nil
}

func (s *Selector) acquirePinnedCapacity(ctx context.Context, value account.Credential) (*accountLease, error) {
	_, _, _, capacityWait := s.routingConfig()
	deadline := time.Now().Add(capacityWait)
	for {
		lease, err := s.claimAccountSlot(ctx, value)
		if err != nil || lease != nil {
			return lease, err
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) leaseReturnNotice() <-chan struct{} {
	s.leaseWakeMu.Lock()
	defer s.leaseWakeMu.Unlock()
	if s.leaseWake == nil {
		s.leaseWake = make(chan struct{})
	}
	return s.leaseWake
}

func (s *Selector) announceLeaseReturn() {
	s.leaseWakeMu.Lock()
	if s.leaseWake != nil {
		close(s.leaseWake)
	}
	s.leaseWake = make(chan struct{})
	s.leaseWakeMu.Unlock()
}

// awaitLeaseRetry 在本实例归还租约时立即重试；短轮询用于感知其他实例释放的共享并发名额。
func (s *Selector) awaitLeaseRetry(ctx context.Context, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, nil
	}
	notice := s.leaseReturnNotice()
	timer := time.NewTimer(min(remaining, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-notice:
		return true, nil
	case <-timer.C:
		return time.Now().Before(deadline), nil
	}
}

func earlierFuture(current, candidate, now time.Time) time.Time {
	if candidate.IsZero() || !now.Before(candidate) {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryDelay(now, retryAt time.Time) time.Duration {
	if retryAt.IsZero() || !now.Before(retryAt) {
		return 0
	}
	return retryAt.Sub(now)
}

func (s *Selector) sortCandidates(ctx context.Context, values []account.RoutingCandidate, now time.Time, tierOrder []account.WebTier, upstreamModels ...string) error {
	upstreamModel := ""
	if len(upstreamModels) > 0 {
		upstreamModel = strings.TrimSpace(upstreamModels[0])
	}
	s.mu.Lock()
	lastSelected := make(map[uint64]time.Time, len(s.lastSelectedAt))
	for id, value := range s.lastSelectedAt {
		lastSelected[id] = value
	}
	performance := make(map[uint64]routePerformance, len(values))
	if upstreamModel != "" {
		for _, candidate := range values {
			key := routePerformanceKey{accountID: candidate.Credential.ID, upstreamModel: upstreamModel}
			if value, ok := s.performance[key]; ok && now.Sub(value.updatedAt) <= routePerformanceTTL {
				performance[candidate.Credential.ID] = value
			}
		}
	}
	s.mu.Unlock()
	for accountID, value := range s.sharedRoutePerformances(ctx, values, upstreamModel, now) {
		performance[accountID] = routePerformance{
			successEWMA: value.SuccessEWMA, latencyEWMA: value.LatencyEWMA,
			samples: int(min(value.Samples, int64(^uint(0)>>1))), updatedAt: value.UpdatedAt,
		}
	}
	remaining := make(map[uint64]float64, len(values))
	fresh := make(map[uint64]bool, len(values))
	quotaRemaining := make(map[uint64]float64, len(values))
	quotaFresh := make(map[uint64]bool, len(values))
	inFlight := make(map[uint64]int, len(values))
	concurrencyKeys := make([]string, 0, len(values))
	for _, candidate := range values {
		concurrencyKeys = append(concurrencyKeys, fmt.Sprintf("account:%d", candidate.Credential.ID))
	}
	concurrencySnapshot := make(map[string]int, len(values))
	batchReader, batched := s.concurrency.(repository.ConcurrencySnapshotReader)
	if batched {
		var err error
		concurrencySnapshot, err = batchReader.CurrentMany(ctx, concurrencyKeys)
		if err != nil {
			return fmt.Errorf("批量读取账号并发租约: %w", err)
		}
	}
	for _, candidate := range values {
		value := candidate.Credential
		key := fmt.Sprintf("account:%d", value.ID)
		current, found := concurrencySnapshot[key]
		if !batched {
			var err error
			current, err = s.concurrency.Current(ctx, key)
			if err != nil {
				return fmt.Errorf("读取账号并发租约: %w", err)
			}
		} else if !found {
			current = 0
		}
		inFlight[value.ID] = current
		if candidate.Billing != nil {
			remaining[value.ID] = candidate.Billing.Remaining()
			fresh[value.ID] = now.Sub(candidate.Billing.SyncedAt) <= 30*time.Minute
		}
		if candidate.QuotaWindow != nil {
			quotaRemaining[value.ID] = float64(max(0, candidate.QuotaWindow.Remaining))
			if candidate.QuotaWindow.Total > 0 {
				quotaRemaining[value.ID] /= float64(candidate.QuotaWindow.Total)
			}
			quotaFresh[value.ID] = candidate.QuotaWindow.SyncedAt != nil && now.Sub(*candidate.QuotaWindow.SyncedAt) <= 30*time.Minute
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		leftCandidate, rightCandidate := values[i], values[j]
		left, right := leftCandidate.Credential, rightCandidate.Credential
		if leftCandidate.SupportsModel != rightCandidate.SupportsModel {
			return leftCandidate.SupportsModel
		}
		if leftCandidate.ModelCapabilityKnown != rightCandidate.ModelCapabilityKnown {
			return leftCandidate.ModelCapabilityKnown
		}
		leftTier, rightTier := tierOrderRank(tierOrder, left.WebTier), tierOrderRank(tierOrder, right.WebTier)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if left.FailureCount != right.FailureCount {
			return left.FailureCount < right.FailureCount
		}
		if fresh[left.ID] != fresh[right.ID] {
			return fresh[left.ID]
		}
		if inFlight[left.ID] != inFlight[right.ID] {
			return inFlight[left.ID] < inFlight[right.ID]
		}
		leftPerformance, leftKnown := performance[left.ID]
		rightPerformance, rightKnown := performance[right.ID]
		leftQuality := routePerformanceQuality(leftPerformance, leftKnown)
		rightQuality := routePerformanceQuality(rightPerformance, rightKnown)
		if leftQuality != rightQuality {
			return leftQuality > rightQuality
		}
		if leftKnown && rightKnown && leftPerformance.latencyEWMA != rightPerformance.latencyEWMA {
			return leftPerformance.latencyEWMA < rightPerformance.latencyEWMA
		}
		if quotaFresh[left.ID] != quotaFresh[right.ID] {
			return quotaFresh[left.ID]
		}
		if quotaRemaining[left.ID] != quotaRemaining[right.ID] {
			return quotaRemaining[left.ID] > quotaRemaining[right.ID]
		}
		if remaining[left.ID] != remaining[right.ID] {
			return remaining[left.ID] > remaining[right.ID]
		}
		if !lastSelected[left.ID].Equal(lastSelected[right.ID]) {
			return lastSelected[left.ID].Before(lastSelected[right.ID])
		}
		return left.ID < right.ID
	})
	return nil
}

func routePerformanceQuality(value routePerformance, known bool) int {
	if !known || value.samples <= 0 {
		return int(routePerformancePrior * 20)
	}
	confidence := min(1, float64(value.samples)/routePerformanceWarmup)
	quality := routePerformancePrior*(1-confidence) + value.successEWMA*confidence
	return int(quality*20 + 0.5)
}

func (s *Selector) resolveTierOrder(provider account.Provider, upstreamModel string) []account.WebTier {
	if s.tierOrders == nil {
		return nil
	}
	return s.tierOrders.TierOrder(provider, upstreamModel)
}

func tierOrderRank(order []account.WebTier, tier account.WebTier) int {
	for index, value := range order {
		if value == tier {
			return index
		}
	}
	return len(order)
}

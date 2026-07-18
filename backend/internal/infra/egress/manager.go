package egress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
const nodeSnapshotTTL = time.Second
const coolingRecoveryProbeInterval = 5 * time.Second
const coolingRecoveryDialTimeout = 500 * time.Millisecond

type Lease struct {
	NodeID             uint64
	NodeName           string
	Scope              domain.Scope
	GroupID            uint64
	AccountKey         string
	StickyKey          string
	CurrentConcurrency int
	ProxyURL           string
	UserAgent          string
	CFCookies          string
	client             requestClient
	browser            *browserClient
	release            func()
}

// UnavailableError means at least one enabled node is configured for the
// requested scope, but every configured node is currently cooling down.
type UnavailableReason string

const (
	UnavailableCooling  UnavailableReason = "cooling"
	UnavailableProxy    UnavailableReason = "proxy_unavailable"
	UnavailableWorker   UnavailableReason = "browser_worker_unavailable"
	UnavailableCapacity UnavailableReason = "capacity"
)

// UnavailableError keeps the failure class so callers can distinguish a dead
// proxy from a shared browser-worker outage.
type UnavailableError struct {
	Scope  domain.Scope
	Reason UnavailableReason
	NodeID uint64
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return "当前没有可用的出口节点"
	}
	switch e.Reason {
	case UnavailableProxy:
		return fmt.Sprintf("%s proxy unavailable", e.Scope)
	case UnavailableWorker:
		return "Grok Web browser worker unavailable"
	case UnavailableCapacity:
		return "出口组并发容量已满"
	}
	return fmt.Sprintf("当前没有可用的 %s 出口节点", e.Scope)
}

type requestClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

func (l *Lease) Do(request *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	return l.client.Do(request)
}
func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// Release is the manager-shaped counterpart used by schedulers that keep
// lease ownership outside request handlers.
func (m *Manager) Release(lease *Lease) {
	if lease != nil {
		lease.Release()
	}
}

type Manager struct {
	repository  repository.EgressRepository
	groups      repository.EgressGroupRepository
	cipher      *security.Cipher
	mu          sync.Mutex
	clients     map[uint64]cachedClient
	inflight    map[uint64]int
	groupLoad   map[uint64]int
	memberLoad  map[groupMemberKey]int
	nodes       map[domain.Scope]cachedNodeSnapshot
	recovery    map[uint64]time.Time
	roundRobin  map[uint64]uint64
	nodeLoads   singleflight.Group
	distributed repository.ConcurrencyLimiter
}

type cachedClient struct {
	fingerprint string
	client      requestClient
	browser     *browserClient
}

type cachedNodeSnapshot struct {
	values    []domain.Node
	expiresAt time.Time
}

type nodeReservation struct {
	nodeID              uint64
	groupID             uint64
	current             int
	distributedReleases []func()
}

type groupMemberKey struct {
	groupID uint64
	nodeID  uint64
}

func NewManager(egressRepository repository.EgressRepository, cipher *security.Cipher, distributed ...repository.ConcurrencyLimiter) *Manager {
	groups, _ := egressRepository.(repository.EgressGroupRepository)
	var limiter repository.ConcurrencyLimiter
	if len(distributed) > 0 {
		limiter = distributed[0]
	}
	return &Manager{repository: egressRepository, groups: groups, cipher: cipher, clients: make(map[uint64]cachedClient), inflight: make(map[uint64]int), groupLoad: make(map[uint64]int), memberLoad: make(map[groupMemberKey]int), nodes: make(map[domain.Scope]cachedNodeSnapshot), recovery: make(map[uint64]time.Time), roundRobin: make(map[uint64]uint64), distributed: limiter}
}

// AcquireGroup selects from an explicitly configured proxy group. A missing
// group repository or an empty group falls back to the legacy scope pool.
func (m *Manager) AcquireGroup(ctx context.Context, groupID uint64, scope domain.Scope, affinity string) (*Lease, error) {
	return m.acquireGroup(ctx, groupID, scope, affinity, make(map[uint64]struct{}))
}

func (m *Manager) acquireScope(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	lease, _, err := m.acquire(ctx, scope, affinity, true)
	return lease, err
}

func (m *Manager) acquireGroup(ctx context.Context, groupID uint64, scope domain.Scope, affinity string, visited map[uint64]struct{}) (*Lease, error) {
	if groupID == 0 || m.groups == nil {
		return m.acquireScope(ctx, scope, affinity)
	}
	if _, exists := visited[groupID]; exists {
		return nil, fmt.Errorf("出口组备用链存在循环")
	}
	visited[groupID] = struct{}{}
	group, err := m.groups.GetEgressGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group.Scope != scope && !(scope == domain.ScopeWebAsset && group.Scope == domain.ScopeWeb) {
		return nil, fmt.Errorf("出口组作用域与请求不匹配")
	}
	if !group.Enabled {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return m.acquireScope(ctx, scope, affinity)
	}
	members, err := m.groups.ListEgressGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return m.acquireScope(ctx, scope, affinity)
	}
	allowed := make(map[uint64]domain.GroupMember, len(members))
	for _, member := range members {
		if member.Enabled {
			allowed[member.NodeID] = member
		}
	}
	if len(allowed) == 0 {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return m.acquireScope(ctx, scope, affinity)
	}
	now := time.Now().UTC()
	nodes, err := m.listNodes(ctx, group.Scope, now)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	groupLoad := m.groupLoad[group.ID]
	m.mu.Unlock()
	if group.MaxConcurrency > 0 && groupLoad >= group.MaxConcurrency {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return nil, &UnavailableError{Scope: scope, Reason: UnavailableCapacity}
	}
	available := make([]domain.Node, 0, len(nodes))
	highestPriority := -int(^uint(0)>>1) - 1
	for _, node := range nodes {
		limit, ok := allowed[node.ID]
		if !ok || !node.Enabled || (node.CooldownUntil != nil && now.Before(*node.CooldownUntil)) {
			continue
		}
		m.mu.Lock()
		load := m.memberLoad[groupMemberKey{groupID: group.ID, nodeID: node.ID}]
		m.mu.Unlock()
		if limit.MaxConcurrency > 0 && load >= limit.MaxConcurrency {
			continue
		}
		if limit.Priority > highestPriority {
			highestPriority = limit.Priority
			available = available[:0]
		}
		if limit.Priority < highestPriority {
			continue
		}
		available = append(available, node)
	}
	if len(available) == 0 {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return m.acquireScope(ctx, scope, affinity)
	}
	selected := m.selectGroupNode(group, available, allowed, affinity)
	reservation, ok, reserveErr := m.reserveGroupNode(ctx, group, selected, allowed)
	if reserveErr != nil {
		return nil, reserveErr
	}
	if !ok {
		// Another request may have claimed the preferred node after the
		// snapshot was read. Try the remaining members under the same atomic
		// reservation check before using the fallback group.
		for _, candidate := range available {
			if candidate.ID == selected.ID {
				continue
			}
			reservation, ok, reserveErr = m.reserveGroupNode(ctx, group, candidate, allowed)
			if reserveErr != nil {
				return nil, reserveErr
			}
			if ok {
				selected = candidate
				break
			}
		}
	}
	if !ok {
		if group.FallbackGroupID != nil {
			return m.acquireGroup(ctx, *group.FallbackGroupID, scope, affinity, visited)
		}
		return nil, &UnavailableError{Scope: scope, Reason: UnavailableCapacity}
	}
	lease, _, err := m.acquireFromNodes(ctx, scope, affinity, []domain.Node{selected}, reservation)
	if lease != nil {
		lease.GroupID = group.ID
		lease.StickyKey = affinity
	}
	return lease, err
}

func (m *Manager) selectGroupNode(group domain.Group, nodes []domain.Node, members map[uint64]domain.GroupMember, affinity string) domain.Node {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	switch group.Strategy {
	case domain.StrategySticky:
		return m.selectNode(nodes, affinity)
	case domain.StrategyWeighted:
		total := uint64(0)
		for _, node := range nodes {
			total += uint64(max(1, members[node.ID].Weight))
		}
		var slot uint64
		if affinity != "" {
			digest := sha256.Sum256([]byte(affinity))
			slot = binary.BigEndian.Uint64(digest[:8]) % total
		} else {
			m.mu.Lock()
			slot = m.roundRobin[group.ID] % total
			m.roundRobin[group.ID]++
			m.mu.Unlock()
		}
		for _, node := range nodes {
			weight := uint64(max(1, members[node.ID].Weight))
			if slot < weight {
				return node
			}
			slot -= weight
		}
		return nodes[0]
	case domain.StrategyRoundRobin:
		m.mu.Lock()
		slot := m.roundRobin[group.ID] % uint64(len(nodes))
		m.roundRobin[group.ID]++
		m.mu.Unlock()
		return nodes[slot]
	default:
		return m.selectNode(nodes, "")
	}
}

func (m *Manager) Acquire(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	if groupID := groupIDFromContext(ctx); groupID != 0 {
		return m.AcquireGroup(ctx, groupID, scope, affinity)
	}
	lease, _, err := m.acquire(ctx, scope, affinity, true)
	return lease, err
}

func (m *Manager) AcquireIfConfigured(ctx context.Context, scope domain.Scope, affinity string) (*Lease, bool, error) {
	if groupID := groupIDFromContext(ctx); groupID != 0 {
		lease, err := m.AcquireGroup(ctx, groupID, scope, affinity)
		if err != nil {
			return nil, false, err
		}
		return lease, true, nil
	}
	return m.acquire(ctx, scope, affinity, false)
}

func (m *Manager) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool) (*Lease, bool, error) {
	now := time.Now().UTC()
	configured := false
	var available []domain.Node
	for _, candidateScope := range fallbackScopes(scope) {
		nodes, err := m.listNodes(ctx, candidateScope, now)
		if err != nil {
			return nil, false, err
		}
		candidateAvailable := make([]domain.Node, 0, len(nodes))
		for _, node := range nodes {
			if !node.Enabled {
				continue
			}
			configured = true
			if node.CooldownUntil != nil && now.Before(*node.CooldownUntil) {
				if recovered, ok := m.tryRecoverCoolingNode(ctx, node, now); ok {
					candidateAvailable = append(candidateAvailable, recovered)
				}
				continue
			}
			candidateAvailable = append(candidateAvailable, node)
		}
		if len(candidateAvailable) > 0 {
			available = candidateAvailable
			break
		}
	}
	if len(available) == 0 {
		if configured {
			return nil, false, &UnavailableError{Scope: scope, Reason: UnavailableCooling}
		}
		if !allowDirect {
			recordSelection(ctx, Selection{NodeName: "direct", Scope: scope})
			return nil, false, nil
		}
		available = []domain.Node{{ID: 0, Name: "direct", Scope: scope, Enabled: true, Health: 1}}
	}
	sort.SliceStable(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	return m.acquireFromNodes(ctx, scope, affinity, available, nil)
}

func (m *Manager) reserveGroupNode(ctx context.Context, group domain.Group, selected domain.Node, members map[uint64]domain.GroupMember) (*nodeReservation, bool, error) {
	m.mu.Lock()
	if m.groupLoad == nil {
		m.groupLoad = make(map[uint64]int)
	}
	if m.memberLoad == nil {
		m.memberLoad = make(map[groupMemberKey]int)
	}
	if group.MaxConcurrency > 0 && m.groupLoad[group.ID] >= group.MaxConcurrency {
		m.mu.Unlock()
		return nil, false, nil
	}
	member := members[selected.ID]
	memberKey := groupMemberKey{groupID: group.ID, nodeID: selected.ID}
	if member.MaxConcurrency > 0 && m.memberLoad[memberKey] >= member.MaxConcurrency {
		m.mu.Unlock()
		return nil, false, nil
	}
	m.inflight[selected.ID]++
	m.groupLoad[group.ID]++
	m.memberLoad[memberKey]++
	reservation := &nodeReservation{nodeID: selected.ID, groupID: group.ID, current: m.inflight[selected.ID]}
	m.mu.Unlock()

	if m.distributed != nil {
		if group.MaxConcurrency > 0 {
			release, acquired, err := m.distributed.Acquire(ctx, fmt.Sprintf("egress-group:%d", group.ID), group.MaxConcurrency)
			if err != nil || !acquired {
				m.releaseNodeReservation(reservation)
				return nil, false, err
			}
			reservation.distributedReleases = append(reservation.distributedReleases, release)
		}
		if member.MaxConcurrency > 0 {
			release, acquired, err := m.distributed.Acquire(ctx, fmt.Sprintf("egress-member:%d:%d", group.ID, selected.ID), member.MaxConcurrency)
			if err != nil || !acquired {
				m.releaseNodeReservation(reservation)
				return nil, false, err
			}
			reservation.distributedReleases = append(reservation.distributedReleases, release)
		}
	}
	return reservation, true, nil
}

func (m *Manager) releaseNodeReservation(reservation *nodeReservation) {
	if reservation == nil {
		return
	}
	m.mu.Lock()
	m.inflight[reservation.nodeID]--
	if m.inflight[reservation.nodeID] <= 0 {
		delete(m.inflight, reservation.nodeID)
	}
	if reservation.groupID != 0 {
		m.groupLoad[reservation.groupID]--
		if m.groupLoad[reservation.groupID] <= 0 {
			delete(m.groupLoad, reservation.groupID)
		}
		key := groupMemberKey{groupID: reservation.groupID, nodeID: reservation.nodeID}
		m.memberLoad[key]--
		if m.memberLoad[key] <= 0 {
			delete(m.memberLoad, key)
		}
	}
	m.mu.Unlock()
	for _, release := range reservation.distributedReleases {
		if release != nil {
			release()
		}
	}
	reservation.distributedReleases = nil
}

func (m *Manager) acquireFromNodes(ctx context.Context, scope domain.Scope, affinity string, available []domain.Node, reservation *nodeReservation) (*Lease, bool, error) {
	selected := m.selectNode(available, affinity)
	proxyURL, err := m.cipher.Decrypt(selected.EncryptedProxyURL)
	if err != nil {
		m.releaseNodeReservation(reservation)
		return nil, false, err
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		m.releaseNodeReservation(reservation)
		return nil, false, err
	}
	cookies := ""
	if scope != domain.ScopeBuild {
		cookies, err = m.cipher.Decrypt(selected.EncryptedCloudflareCookie)
		if err != nil {
			m.releaseNodeReservation(reservation)
			return nil, false, err
		}
		cookies = application.SanitizeCloudflareCookies(cookies)
	}
	userAgent := ""
	if scope != domain.ScopeBuild {
		userAgent = strings.TrimSpace(selected.UserAgent)
	}
	if scope != domain.ScopeBuild && userAgent == "" {
		userAgent = DefaultUserAgent
	}
	client, err := m.clientFor(selected.ID, scope, proxyURL, userAgent, cookies)
	if err != nil {
		m.releaseNodeReservation(reservation)
		return nil, false, err
	}
	currentConcurrency := 0
	if reservation != nil {
		if reservation.nodeID != selected.ID {
			m.releaseNodeReservation(reservation)
			return nil, false, errors.New("出口节点预占与选择结果不一致")
		}
		currentConcurrency = reservation.current
	} else {
		m.mu.Lock()
		m.inflight[selected.ID]++
		currentConcurrency = m.inflight[selected.ID]
		m.mu.Unlock()
	}
	recordSelection(ctx, Selection{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, Proxied: proxyURL != ""})
	var once sync.Once
	groupID := uint64(0)
	if reservation != nil {
		groupID = reservation.groupID
	}
	return &Lease{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, GroupID: groupID, AccountKey: affinity, StickyKey: affinity, CurrentConcurrency: currentConcurrency, ProxyURL: proxyURL, UserAgent: userAgent, CFCookies: cookies, client: client.client, browser: client.browser, release: func() {
		once.Do(func() {
			if reservation != nil {
				m.releaseNodeReservation(reservation)
				return
			}
			m.mu.Lock()
			m.inflight[selected.ID]--
			if m.inflight[selected.ID] <= 0 {
				delete(m.inflight, selected.ID)
			}
			m.mu.Unlock()
		})
	}}, true, nil
}

// tryRecoverCoolingNode lets a restarted proxy rejoin before its exponential
// cooldown expires. Probes are throttled and only test the configured proxy's
// TCP listener; no upstream request or credential is sent.
func (m *Manager) tryRecoverCoolingNode(ctx context.Context, node domain.Node, now time.Time) (domain.Node, bool) {
	if node.ID == 0 || node.EncryptedProxyURL == "" || m.cipher == nil || !m.claimRecoveryProbe(node.ID, now) {
		return domain.Node{}, false
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return domain.Node{}, false
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		return domain.Node{}, false
	}
	address, err := proxyDialAddress(proxyURL)
	if err != nil {
		return domain.Node{}, false
	}
	connection, err := (&net.Dialer{Timeout: coolingRecoveryDialTimeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return domain.Node{}, false
	}
	_ = connection.Close()
	node.Health = max(0.5, node.Health)
	node.FailureCount = 0
	node.CooldownUntil = nil
	node.LastError = ""
	node.UpdatedAt = now
	updated, err := m.repository.UpdateEgressNode(ctx, node)
	if err != nil {
		return domain.Node{}, false
	}
	m.mu.Lock()
	delete(m.recovery, node.ID)
	m.invalidateClientLocked(node.ID)
	m.mu.Unlock()
	m.invalidateNodes(node.Scope)
	return updated, true
}

func (m *Manager) claimRecoveryProbe(nodeID uint64, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recovery == nil {
		m.recovery = make(map[uint64]time.Time)
	}
	if previous := m.recovery[nodeID]; !previous.IsZero() && now.Sub(previous) < coolingRecoveryProbeInterval {
		return false
	}
	m.recovery[nodeID] = now
	return true
}

func proxyDialAddress(proxyURL string) (string, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("代理地址格式无效")
	}
	port := parsed.Port()
	if port == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

func (m *Manager) listNodes(ctx context.Context, scope domain.Scope, now time.Time) ([]domain.Node, error) {
	m.mu.Lock()
	if snapshot, ok := m.nodes[scope]; ok && now.Before(snapshot.expiresAt) {
		values := append([]domain.Node(nil), snapshot.values...)
		m.mu.Unlock()
		return values, nil
	}
	m.mu.Unlock()
	loaded, err, _ := m.nodeLoads.Do(string(scope), func() (any, error) {
		checkTime := time.Now().UTC()
		m.mu.Lock()
		if snapshot, ok := m.nodes[scope]; ok && checkTime.Before(snapshot.expiresAt) {
			values := append([]domain.Node(nil), snapshot.values...)
			m.mu.Unlock()
			return values, nil
		}
		m.mu.Unlock()
		values, err := m.repository.ListEgressNodes(ctx, scope, repository.SortQuery{})
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.nodes[scope] = cachedNodeSnapshot{values: append([]domain.Node(nil), values...), expiresAt: checkTime.Add(nodeSnapshotTTL)}
		m.mu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]domain.Node(nil), loaded.([]domain.Node)...), nil
}

func (m *Manager) invalidateNodes(scope domain.Scope) {
	m.mu.Lock()
	delete(m.nodes, scope)
	m.mu.Unlock()
}

func fallbackScopes(scope domain.Scope) []domain.Scope {
	if scope == domain.ScopeWebAsset {
		return []domain.Scope{domain.ScopeWebAsset, domain.ScopeWeb}
	}
	return []domain.Scope{scope}
}

func (m *Manager) selectNode(nodes []domain.Node, affinity string) domain.Node {
	if affinity != "" {
		digest := sha256.Sum256([]byte(affinity))
		selected := nodes[int(binary.BigEndian.Uint64(digest[:8])%uint64(len(nodes)))]
		if selected.Health >= 0.8 || len(nodes) == 1 {
			return selected
		}
		for _, node := range nodes {
			if node.Health > selected.Health {
				selected = node
			}
		}
		return selected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	best := nodes[0]
	for _, node := range nodes[1:] {
		if m.inflight[node.ID] < m.inflight[best.ID] || (m.inflight[node.ID] == m.inflight[best.ID] && node.Health > best.Health) {
			best = node
		}
	}
	return best
}

func (m *Manager) clientFor(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string) (cachedClient, error) {
	clientKind := "browser"
	if scope == domain.ScopeBuild {
		clientKind = "build"
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(clientKind+"\x00"+proxyURL+"\x00"+userAgent+"\x00"+cookies)))
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.clients[id]; ok && cached.fingerprint == fingerprint {
		return cached, nil
	}
	var value cachedClient
	value.fingerprint = fingerprint
	if scope == domain.ScopeBuild {
		client, err := newBuildClient(proxyURL)
		if err != nil {
			return cachedClient{}, err
		}
		value.client = client
	} else {
		client, err := newBrowserClient(proxyURL)
		if err != nil {
			return cachedClient{}, err
		}
		value.client = client
		value.browser = client
	}
	if previous, exists := m.clients[id]; exists && previous.client != nil {
		previous.client.CloseIdleConnections()
	}
	m.clients[id] = value
	return value, nil
}

func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

// FeedbackLease preserves the group/lease call shape for registration and
// other schedulers while keeping the existing node feedback implementation.
func (m *Manager) FeedbackLease(ctx context.Context, lease *Lease, status int, transportErr error) {
	if lease == nil {
		return
	}
	m.FeedbackForScope(ctx, lease.Scope, lease.NodeID, status, transportErr)
}

func (m *Manager) FeedbackForScope(ctx context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	if nodeID == 0 {
		if transportErr != nil || status >= 500 || (scope != domain.ScopeBuild && status == http.StatusForbidden) {
			m.mu.Lock()
			m.invalidateClientLocked(0)
			m.mu.Unlock()
		}
		return
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	switch {
	case transportErr == nil && status >= 200 && status < 400:
		value.Health = min(1, value.Health+0.1)
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
	case status == http.StatusUnauthorized || status == http.StatusTooManyRequests:
		return
	case scope == domain.ScopeBuild && status == http.StatusForbidden:
		// Build 403 可能是账号权限、额度、Token 或出口策略，响应体由网关层
		// 分类；仅凭状态码不能把标准 CLI 出口误判为 Web anti-bot。
		return
	case status == http.StatusForbidden:
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		value.CooldownUntil = nil
		value.LastError = "anti-bot rejection"
		m.mu.Lock()
		m.invalidateClientLocked(nodeID)
		m.mu.Unlock()
	default:
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		cooldown := min(10*time.Minute, 30*time.Second*time.Duration(1<<min(value.FailureCount-1, 4)))
		until := now.Add(cooldown)
		value.CooldownUntil = &until
		if transportErr != nil {
			value.LastError = "transport error"
		} else {
			value.LastError = fmt.Sprintf("upstream status %d", status)
		}
		m.mu.Lock()
		m.invalidateClientLocked(nodeID)
		m.mu.Unlock()
	}
	if _, err := m.repository.UpdateEgressNode(ctx, value); err == nil {
		m.invalidateNodes(value.Scope)
	}
}

func (m *Manager) invalidateClientLocked(nodeID uint64) {
	if cached, exists := m.clients[nodeID]; exists && cached.client != nil {
		cached.client.CloseIdleConnections()
	}
	delete(m.clients, nodeID)
}

func BuildSSOCookie(token, cloudflareCookies string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "sso=") {
		token = strings.TrimSpace(token[len("sso="):])
	}
	if value, _, found := strings.Cut(token, ";"); found {
		token = strings.TrimSpace(value)
	}
	token = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(token)
	cookies := "sso=" + token + "; sso-rw=" + token
	if sanitized := application.SanitizeCloudflareCookies(cloudflareCookies); sanitized != "" {
		cookies += "; " + sanitized
	}
	return cookies
}

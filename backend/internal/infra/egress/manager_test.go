package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	domain "github.com/owen891/grok2api/backend/internal/domain/egress"
	memoryruntime "github.com/owen891/grok2api/backend/internal/infra/runtime/memory"
	"github.com/owen891/grok2api/backend/internal/infra/security"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestDirectFallbackRebuildsClientAfterAntiBotRejection(t *testing.T) {
	manager := &Manager{clients: map[uint64]cachedClient{0: {}}}
	manager.Feedback(context.Background(), 0, http.StatusForbidden, nil)
	if _, exists := manager.clients[0]; exists {
		t.Fatal("direct fallback client was not invalidated after anti-bot rejection")
	}
}

func TestBrowserRequestLeavesHeaderOrderingToTLSProfile(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://grok.com/rest/app-chat/conversations/new", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("User-Agent", DefaultUserAgent)
	request.Header.Set("Accept", "*/*")
	converted, err := toFHTTPRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Header[fhttp.HeaderOrderKey]) != 0 || len(converted.Header[fhttp.PHeaderOrderKey]) != 0 {
		t.Fatalf("manual header order=%#v pseudo=%#v", converted.Header[fhttp.HeaderOrderKey], converted.Header[fhttp.PHeaderOrderKey])
	}
}

func TestConfiguredCoolingAppNodesNeverFallBackToDirect(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "proxy", Scope: domain.ScopeWeb, Enabled: true, CooldownUntil: &until,
	}}}, cipher)
	if _, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account"); err == nil {
		t.Fatal("cooling configured node unexpectedly fell back to direct")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) || unavailable.Scope != domain.ScopeWeb {
			t.Fatalf("error = %T %v", err, err)
		}
	}
}

func TestCoolingProxyRecoversAsSoonAsListenerReturns(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	encryptedProxy, err := cipher.Encrypt("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "recovering-proxy", Scope: domain.ScopeBuild, Enabled: true, Health: 0.2,
		FailureCount: 3, CooldownUntil: &until, LastError: "transport error", EncryptedProxyURL: encryptedProxy,
	}}
	manager := NewManager(repository, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeBuild, "account")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if repository.updates != 1 || repository.node.CooldownUntil != nil || repository.node.FailureCount != 0 || repository.node.LastError != "" || repository.node.Health < 0.5 {
		t.Fatalf("recovered node = %#v, updates=%d", repository.node, repository.updates)
	}
}

func TestDisabledConfiguredNodesFallBackToDirect(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "disabled-proxy", Scope: domain.ScopeWeb, Enabled: false,
	}}}, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 0 || lease.NodeName != "direct" || lease.ProxyURL != "" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestAcquireIfConfiguredDoesNotChangeBuildDirectTransport(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{}, cipher)
	ctx, trace := WithTrace(context.Background())
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "")
	if err != nil || configured || lease != nil {
		t.Fatalf("lease=%#v configured=%v err=%v", lease, configured, err)
	}
	selection, ok := trace.Selection(domain.ScopeBuild)
	if !ok || selection.NodeID != 0 || selection.NodeName != "direct" || selection.Proxied {
		t.Fatalf("direct selection = %#v, ok=%v", selection, ok)
	}
}

func TestAcquireIfConfiguredHonorsBuildGroupFromContext(t *testing.T) {
	manager := newGroupManager(t, domain.Group{ID: 7, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyRoundRobin}, []domain.GroupMember{
		{GroupID: 7, NodeID: 2, Enabled: true, Weight: 1},
	})
	ctx := WithGroupID(context.Background(), 7)
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	if !configured || lease == nil || lease.NodeID != 2 || lease.GroupID != 7 {
		t.Fatalf("lease=%#v configured=%v", lease, configured)
	}
	lease.Release()
}

func TestTraceRecordsConfiguredProxyWithoutCredentials(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://secret:password@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 42, Name: "primary-proxy", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy,
	}}}, cipher)
	ctx, trace := WithTrace(context.Background())
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "")
	if err != nil || !configured || lease == nil {
		t.Fatalf("lease=%#v configured=%v err=%v", lease, configured, err)
	}
	defer lease.Release()
	selection, ok := trace.Selection(domain.ScopeBuild)
	if !ok || selection.NodeID != 42 || selection.NodeName != "primary-proxy" || !selection.Proxied {
		t.Fatalf("proxy selection = %#v, ok=%v", selection, ok)
	}
}

func TestConfiguredBuildNodeDoesNotOverrideProviderUserAgent(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://warp:1080")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1, UserAgent: "legacy-build-agent", EncryptedProxyURL: encryptedProxy,
	}}}, cipher)
	lease, configured, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	if !configured || lease == nil {
		t.Fatal("configured build node did not produce a lease")
	}
	defer lease.Release()
	if lease.UserAgent != "" {
		t.Fatalf("build lease userAgent = %q", lease.UserAgent)
	}
	if _, ok := lease.client.(*http.Client); !ok || lease.browser != nil || lease.Scope != domain.ScopeBuild {
		t.Fatalf("build lease client=%T browser=%p scope=%q", lease.client, lease.browser, lease.Scope)
	}
	if _, _, err := lease.DialWebSocket(context.Background(), "wss://example.com", nil, time.Second); err == nil {
		t.Fatal("build lease unexpectedly exposed browser WebSocket")
	}
}

func TestConfiguredWebNodeKeepsChromeBrowserTransport(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{{
		ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1,
	}}}, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, ok := lease.client.(*browserClient); !ok || lease.browser == nil || lease.Scope != domain.ScopeWeb {
		t.Fatalf("web lease client=%T browser=%p scope=%q", lease.client, lease.browser, lease.Scope)
	}
}

func TestBuildForbiddenDoesNotPoisonEgressNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "build", Scope: domain.ScopeBuild, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	lease, _, err := manager.AcquireIfConfigured(context.Background(), domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	manager.FeedbackForScope(context.Background(), domain.ScopeBuild, 1, http.StatusForbidden, nil)
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.LastError != "" {
		t.Fatalf("build 403 poisoned node: updates=%d node=%#v", repository.updates, repository.node)
	}
	if _, exists := manager.clients[1]; !exists {
		t.Fatal("build client was invalidated by an ambiguous 403")
	}
}

func TestWebForbiddenStillRebuildsBrowserSession(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &mutableEgressRepository{node: domain.Node{ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1}}
	manager := NewManager(repository, cipher)
	lease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	manager.Feedback(context.Background(), 1, http.StatusForbidden, nil)
	if repository.updates != 1 || repository.node.Health >= 1 || repository.node.LastError != "anti-bot rejection" {
		t.Fatalf("web 403 feedback = updates=%d node=%#v", repository.updates, repository.node)
	}
	if _, exists := manager.clients[1]; exists {
		t.Fatal("web browser session was not invalidated after 403")
	}
}

func TestUpdateCloudflareSessionSanitizesAndRestoresNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	oldCookie, err := cipher.Encrypt("cf_clearance=stale")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	repository := &mutableEgressRepository{node: domain.Node{
		ID: 1, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 0.05, FailureCount: 9,
		CooldownUntil: &until, LastError: "anti-bot rejection", EncryptedCloudflareCookie: oldCookie,
	}}
	manager := NewManager(repository, cipher)
	manager.clients[1] = cachedClient{}
	if err := manager.UpdateCloudflareSession(context.Background(), 1, "cf_clearance=fresh; sso=secret; __cf_bm=bm", "Chrome/Fresh"); err != nil {
		t.Fatal(err)
	}
	cookies, err := cipher.Decrypt(repository.node.EncryptedCloudflareCookie)
	if err != nil {
		t.Fatal(err)
	}
	if cookies != "cf_clearance=fresh; __cf_bm=bm" || repository.node.UserAgent != "Chrome/Fresh" {
		t.Fatalf("session cookies=%q userAgent=%q", cookies, repository.node.UserAgent)
	}
	if repository.updates != 1 || repository.node.Health != 1 || repository.node.FailureCount != 0 || repository.node.CooldownUntil != nil || repository.node.LastError != "" {
		t.Fatalf("restored node=%#v updates=%d", repository.node, repository.updates)
	}
	if _, exists := manager.clients[1]; exists {
		t.Fatal("stale egress client was not invalidated")
	}
}

func TestWebAssetFallsBackToWeb(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 2, Name: "web", Scope: domain.ScopeWeb, Enabled: true, Health: 1},
	}}, cipher)
	webLease, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer webLease.Release()
	lease, err := manager.Acquire(context.Background(), domain.ScopeWebAsset, "account")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 {
		t.Fatalf("node = %d, want web fallback node 2", lease.NodeID)
	}
	if lease.client != webLease.client {
		t.Fatal("Web Asset fallback did not reuse the matching Web browser session")
	}
}

func TestEgressNodeSnapshotAvoidsRepeatedRepositoryReads(t *testing.T) {
	repository := &countingEgressRepository{egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{{ID: 1, Scope: domain.ScopeWeb, Enabled: true}}}}
	manager := NewManager(repository, nil)
	now := time.Now().UTC()
	for range 2 {
		values, err := manager.listNodes(context.Background(), domain.ScopeWeb, now)
		if err != nil || len(values) != 1 {
			t.Fatalf("nodes=%#v err=%v", values, err)
		}
	}
	if repository.calls != 1 {
		t.Fatalf("repository reads = %d, want 1", repository.calls)
	}
}

func TestAcquireGroupRoundRobin(t *testing.T) {
	manager := newGroupManager(t, domain.Group{ID: 1, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyRoundRobin}, []domain.GroupMember{
		{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1},
		{GroupID: 1, NodeID: 2, Enabled: true, Weight: 1},
	})
	var selected []uint64
	for range 4 {
		lease, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, lease.NodeID)
		lease.Release()
	}
	want := []uint64{1, 2, 1, 2}
	for index := range want {
		if selected[index] != want[index] {
			t.Fatalf("round robin = %v, want %v", selected, want)
		}
	}
}

func TestAcquireGroupWeighted(t *testing.T) {
	manager := newGroupManager(t, domain.Group{ID: 1, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyWeighted}, []domain.GroupMember{
		{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1},
		{GroupID: 1, NodeID: 2, Enabled: true, Weight: 3},
	})
	counts := map[uint64]int{}
	for range 8 {
		lease, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
		if err != nil {
			t.Fatal(err)
		}
		counts[lease.NodeID]++
		lease.Release()
	}
	if counts[1] != 2 || counts[2] != 6 {
		t.Fatalf("weighted counts = %v", counts)
	}
}

func TestAcquireGroupStickyKeepsAffinity(t *testing.T) {
	manager := newGroupManager(t, domain.Group{ID: 1, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategySticky}, []domain.GroupMember{
		{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1},
		{GroupID: 1, NodeID: 2, Enabled: true, Weight: 1},
	})
	first, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "stable-account")
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.NodeID
	first.Release()
	for range 3 {
		lease, acquireErr := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "stable-account")
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		if lease.NodeID != firstID {
			t.Fatalf("sticky node = %d, want %d", lease.NodeID, firstID)
		}
		lease.Release()
	}
}

func TestAcquireGroupFallsBackAtConcurrencyLimit(t *testing.T) {
	fallbackID := uint64(2)
	repository := &groupEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: testGroupNodes()},
		groups: map[uint64]domain.Group{
			1: {ID: 1, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyLeastLoad, MaxConcurrency: 1, FallbackGroupID: &fallbackID},
			2: {ID: 2, Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyLeastLoad},
		},
		members: map[uint64][]domain.GroupMember{
			1: {{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1}},
			2: {{GroupID: 2, NodeID: 2, Enabled: true, Weight: 1}},
		},
	}
	manager := NewManager(repository, testCipher(t))
	first, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first.NodeID != 1 || second.NodeID != 2 {
		t.Fatalf("primary/fallback nodes = %d/%d", first.NodeID, second.NodeID)
	}
}

func TestAcquireGroupConcurrencyReservationIsAtomic(t *testing.T) {
	manager := newGroupManager(t, domain.Group{ID: 1, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1}, []domain.GroupMember{
		{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1, MaxConcurrency: 1},
	})
	const callers = 32
	start := make(chan struct{})
	leases := make(chan *Lease, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			lease, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
			if err == nil {
				leases <- lease
			}
		}()
	}
	close(start)
	wait.Wait()
	close(leases)
	count := 0
	for lease := range leases {
		count++
		lease.Release()
	}
	if count != 1 {
		t.Fatalf("successful leases = %d, want exactly one", count)
	}
}

func TestAcquireGroupMemberCapacityIsScopedToGroup(t *testing.T) {
	repository := &groupEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: testGroupNodes()},
		groups: map[uint64]domain.Group{
			1: {ID: 1, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1},
			2: {ID: 2, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1},
		},
		members: map[uint64][]domain.GroupMember{
			1: {{GroupID: 1, NodeID: 1, Enabled: true, MaxConcurrency: 1}},
			2: {{GroupID: 2, NodeID: 1, Enabled: true, MaxConcurrency: 1}},
		},
	}
	manager := NewManager(repository, testCipher(t))
	first, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := manager.AcquireGroup(context.Background(), 2, domain.ScopeBuild, "")
	if err != nil {
		t.Fatalf("shared node was incorrectly counted against another group: %v", err)
	}
	second.Release()
}

func TestAcquireGroupUsesDistributedCapacityLimiter(t *testing.T) {
	repository := &groupEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: testGroupNodes()},
		groups:                   map[uint64]domain.Group{1: {ID: 1, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1}},
		members:                  map[uint64][]domain.GroupMember{1: {{GroupID: 1, NodeID: 1, Enabled: true, MaxConcurrency: 1}}},
	}
	limiter := memoryruntime.NewConcurrencyLimiter()
	managerOne := NewManager(repository, testCipher(t), limiter)
	managerTwo := NewManager(repository, testCipher(t), limiter)
	lease, err := managerOne.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := managerTwo.AcquireGroup(context.Background(), 1, domain.ScopeBuild, ""); err == nil {
		t.Fatal("distributed capacity limit was ignored")
	}
}

func TestAcquireGroupDetectsFallbackCycle(t *testing.T) {
	groupOneID, groupTwoID := uint64(1), uint64(2)
	repository := &groupEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: testGroupNodes()},
		groups: map[uint64]domain.Group{
			1: {ID: 1, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1, FallbackGroupID: &groupTwoID},
			2: {ID: 2, Scope: domain.ScopeBuild, Enabled: true, MaxConcurrency: 1, FallbackGroupID: &groupOneID},
		},
		members: map[uint64][]domain.GroupMember{
			1: {{GroupID: 1, NodeID: 1, Enabled: true, Weight: 1}},
			2: {{GroupID: 2, NodeID: 1, Enabled: true, Weight: 1}},
		},
	}
	manager := NewManager(repository, testCipher(t))
	lease, err := manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	fallbackLease, err := manager.AcquireGroup(context.Background(), 2, domain.ScopeBuild, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fallbackLease.Release()
	if _, err = manager.AcquireGroup(context.Background(), 1, domain.ScopeBuild, ""); err == nil {
		t.Fatal("fallback cycle was accepted")
	}
}

func newGroupManager(t *testing.T, group domain.Group, members []domain.GroupMember) *Manager {
	t.Helper()
	repository := &groupEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: testGroupNodes()},
		groups:                   map[uint64]domain.Group{group.ID: group},
		members:                  map[uint64][]domain.GroupMember{group.ID: members},
	}
	return NewManager(repository, testCipher(t))
}

func testGroupNodes() []domain.Node {
	return []domain.Node{
		{ID: 1, Name: "one", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
		{ID: 2, Name: "two", Scope: domain.ScopeBuild, Enabled: true, Health: 1},
	}
}

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

type groupEgressRepository struct {
	egressRepositoryTestStub
	groups  map[uint64]domain.Group
	members map[uint64][]domain.GroupMember
}

func (r *groupEgressRepository) ListEgressGroups(_ context.Context, scope domain.Scope) ([]domain.Group, error) {
	values := make([]domain.Group, 0, len(r.groups))
	for _, group := range r.groups {
		if scope == "" || group.Scope == scope {
			values = append(values, group)
		}
	}
	return values, nil
}

func (r *groupEgressRepository) GetEgressGroup(_ context.Context, id uint64) (domain.Group, error) {
	value, ok := r.groups[id]
	if !ok {
		return domain.Group{}, repository.ErrNotFound
	}
	return value, nil
}

func (r *groupEgressRepository) CreateEgressGroup(_ context.Context, value domain.Group) (domain.Group, error) {
	r.groups[value.ID] = value
	return value, nil
}

func (r *groupEgressRepository) UpdateEgressGroup(_ context.Context, value domain.Group) (domain.Group, error) {
	r.groups[value.ID] = value
	return value, nil
}

func (r *groupEgressRepository) DeleteEgressGroup(_ context.Context, id uint64) error {
	delete(r.groups, id)
	return nil
}

func (r *groupEgressRepository) ListEgressGroupMembers(_ context.Context, groupID uint64) ([]domain.GroupMember, error) {
	return r.members[groupID], nil
}

func (r *groupEgressRepository) UpsertEgressGroupMember(_ context.Context, value domain.GroupMember) (domain.GroupMember, error) {
	r.members[value.GroupID] = append(r.members[value.GroupID], value)
	return value, nil
}

func (r *groupEgressRepository) DeleteEgressGroupMember(_ context.Context, groupID, nodeID uint64) error {
	values := r.members[groupID]
	for index, value := range values {
		if value.NodeID == nodeID {
			r.members[groupID] = append(values[:index], values[index+1:]...)
			return nil
		}
	}
	return repository.ErrNotFound
}

type egressRepositoryTestStub struct{ nodes []domain.Node }

type countingEgressRepository struct {
	egressRepositoryTestStub
	calls int
}

type mutableEgressRepository struct {
	node    domain.Node
	updates int
}

func (r *mutableEgressRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	if scope != "" && r.node.Scope != scope {
		return nil, nil
	}
	return []domain.Node{r.node}, nil
}

func (r *mutableEgressRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if r.node.ID != id {
		return domain.Node{}, errors.New("not found")
	}
	return r.node, nil
}

func (r *mutableEgressRepository) CreateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.node = value
	return value, nil
}

func (r *mutableEgressRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.node = value
	r.updates++
	return value, nil
}

func (r *mutableEgressRepository) DeleteEgressNode(_ context.Context, id uint64) error {
	if r.node.ID != id {
		return errors.New("not found")
	}
	r.node = domain.Node{}
	return nil
}

func (r *countingEgressRepository) ListEgressNodes(ctx context.Context, scope domain.Scope, sort repository.SortQuery) ([]domain.Node, error) {
	r.calls++
	return r.egressRepositoryTestStub.ListEgressNodes(ctx, scope, sort)
}

func (s egressRepositoryTestStub) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	values := make([]domain.Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		if scope == "" || node.Scope == scope {
			values = append(values, node)
		}
	}
	return values, nil
}
func (egressRepositoryTestStub) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return domain.Node{}, errors.New("not found")
}
func (egressRepositoryTestStub) CreateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, errors.New("unsupported")
}
func (egressRepositoryTestStub) UpdateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, errors.New("unsupported")
}
func (egressRepositoryTestStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

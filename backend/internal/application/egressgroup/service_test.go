package egressgroup

import (
	"context"
	"errors"
	"testing"

	domain "github.com/owen891/grok2api/backend/internal/domain/egress"
	"github.com/owen891/grok2api/backend/internal/infra/security"
	"github.com/owen891/grok2api/backend/internal/repository"
)

type groupRepoStub struct {
	group   domain.Group
	groups  map[uint64]domain.Group
	members []domain.GroupMember
	nodes   []domain.Node
}

func (r *groupRepoStub) ListEgressGroups(context.Context, domain.Scope) ([]domain.Group, error) {
	return []domain.Group{r.group}, nil
}
func (r *groupRepoStub) GetEgressGroup(_ context.Context, id uint64) (domain.Group, error) {
	if r.groups != nil {
		value, ok := r.groups[id]
		if !ok {
			return domain.Group{}, repository.ErrNotFound
		}
		return value, nil
	}
	return r.group, nil
}
func (r *groupRepoStub) CreateEgressGroup(_ context.Context, v domain.Group) (domain.Group, error) {
	v.ID = 1
	r.group = v
	return v, nil
}
func (r *groupRepoStub) UpdateEgressGroup(_ context.Context, v domain.Group) (domain.Group, error) {
	r.group = v
	return v, nil
}
func (r *groupRepoStub) DeleteEgressGroup(context.Context, uint64) error { return nil }
func (r *groupRepoStub) ListEgressGroupMembers(context.Context, uint64) ([]domain.GroupMember, error) {
	return r.members, nil
}
func (r *groupRepoStub) UpsertEgressGroupMember(_ context.Context, v domain.GroupMember) (domain.GroupMember, error) {
	r.members = append(r.members, v)
	return v, nil
}
func (r *groupRepoStub) DeleteEgressGroupMember(context.Context, uint64, uint64) error { return nil }
func (r *groupRepoStub) ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error) {
	return r.nodes, nil
}
func (r *groupRepoStub) CreateEgressNode(_ context.Context, v domain.Node) (domain.Node, error) {
	v.ID = uint64(len(r.nodes) + 1)
	r.nodes = append(r.nodes, v)
	return v, nil
}

func TestImportCreatesAndReusesNormalizedProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &groupRepoStub{group: domain.Group{ID: 1, Scope: domain.ScopeBuild}}
	results, err := NewService(repo, repo, cipher).Import(context.Background(), 1, []ImportLine{{Line: 1, Value: "socks5://user:pass@127.0.0.1:1080"}, {Line: 2, Value: "socks5://user:pass@127.0.0.1:1080"}}, false, MemberInput{Enabled: true, Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Created || !results[1].Reused || len(repo.nodes) != 1 {
		t.Fatalf("results=%+v nodes=%+v", results, repo.nodes)
	}
}

func TestImportDryRunDoesNotPersist(t *testing.T) {
	cipher, _ := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	repo := &groupRepoStub{group: domain.Group{ID: 1, Scope: domain.ScopeBuild}}
	results, err := NewService(repo, repo, cipher).Import(context.Background(), 1, []ImportLine{{Line: 1, Value: "http://127.0.0.1:8080"}}, true, MemberInput{Enabled: true, Weight: 1})
	if err != nil || len(results) != 1 || len(repo.nodes) != 0 {
		t.Fatalf("results=%+v err=%v nodes=%+v", results, err, repo.nodes)
	}
}

func TestCreateRejectsMissingAndMismatchedFallbackGroups(t *testing.T) {
	buildFallback := uint64(2)
	repo := &groupRepoStub{groups: map[uint64]domain.Group{
		2: {ID: 2, Scope: domain.ScopeWeb},
	}}
	service := NewService(repo, repo, nil)
	for _, input := range []Input{
		{Name: "missing", Scope: domain.ScopeBuild, Enabled: true, FallbackGroupID: uint64Pointer(99)},
		{Name: "mismatch", Scope: domain.ScopeBuild, Enabled: true, FallbackGroupID: &buildFallback},
	} {
		if _, err := service.Create(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Create(%q) error = %v", input.Name, err)
		}
	}
}

func TestUpdateRejectsFallbackCycle(t *testing.T) {
	groupOne := uint64(1)
	groupTwo := uint64(2)
	repo := &groupRepoStub{groups: map[uint64]domain.Group{
		1: {ID: 1, Name: "one", Scope: domain.ScopeBuild},
		2: {ID: 2, Name: "two", Scope: domain.ScopeBuild, FallbackGroupID: &groupOne},
	}}
	_, err := NewService(repo, repo, nil).Update(context.Background(), 1, Input{
		Name: "one", Scope: domain.ScopeBuild, Enabled: true, Strategy: domain.StrategyLeastLoad, FallbackGroupID: &groupTwo,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Update() error = %v", err)
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

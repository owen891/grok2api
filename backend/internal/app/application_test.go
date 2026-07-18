package app

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type registrationProxyRepository struct {
	groups  map[uint64]domain.Group
	members map[uint64][]domain.GroupMember
	nodes   []domain.Node
}

func (r *registrationProxyRepository) ListEgressGroups(context.Context, domain.Scope) ([]domain.Group, error) {
	return nil, nil
}
func (r *registrationProxyRepository) GetEgressGroup(_ context.Context, id uint64) (domain.Group, error) {
	return r.groups[id], nil
}
func (r *registrationProxyRepository) CreateEgressGroup(context.Context, domain.Group) (domain.Group, error) {
	return domain.Group{}, nil
}
func (r *registrationProxyRepository) UpdateEgressGroup(context.Context, domain.Group) (domain.Group, error) {
	return domain.Group{}, nil
}
func (r *registrationProxyRepository) DeleteEgressGroup(context.Context, uint64) error { return nil }
func (r *registrationProxyRepository) ListEgressGroupMembers(_ context.Context, id uint64) ([]domain.GroupMember, error) {
	return r.members[id], nil
}
func (r *registrationProxyRepository) UpsertEgressGroupMember(context.Context, domain.GroupMember) (domain.GroupMember, error) {
	return domain.GroupMember{}, nil
}
func (r *registrationProxyRepository) DeleteEgressGroupMember(context.Context, uint64, uint64) error {
	return nil
}
func (r *registrationProxyRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	values := make([]domain.Node, 0, len(r.nodes))
	for _, value := range r.nodes {
		if value.Scope == scope {
			values = append(values, value)
		}
	}
	return values, nil
}
func (r *registrationProxyRepository) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return domain.Node{}, nil
}
func (r *registrationProxyRepository) CreateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, nil
}
func (r *registrationProxyRepository) UpdateEgressNode(context.Context, domain.Node) (domain.Node, error) {
	return domain.Node{}, nil
}
func (r *registrationProxyRepository) DeleteEgressNode(context.Context, uint64) error { return nil }

func TestResolveRegistrationProxyGroupUsesFallbackPool(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("http://fallback-proxy:8080")
	if err != nil {
		t.Fatal(err)
	}
	fallbackID := uint64(2)
	repo := &registrationProxyRepository{
		groups: map[uint64]domain.Group{
			1: {ID: 1, Scope: domain.ScopeBuild, Enabled: true, FallbackGroupID: &fallbackID},
			2: {ID: 2, Scope: domain.ScopeBuild, Enabled: true},
		},
		members: map[uint64][]domain.GroupMember{
			1: {{GroupID: 1, NodeID: 1, Enabled: true}},
			2: {{GroupID: 2, NodeID: 2, Enabled: true}},
		},
		nodes: []domain.Node{
			{ID: 1, Scope: domain.ScopeBuild, Enabled: true},
			{ID: 2, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: proxy},
		},
	}
	values, err := resolveRegistrationProxyGroup(context.Background(), repo, cipher, 1, "grok_build", make(map[uint64]struct{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != "http://fallback-proxy:8080" {
		t.Fatalf("resolved proxy pool = %#v", values)
	}
}

func TestResolveRegistrationProxyGroupRejectsScopeMismatch(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repo := &registrationProxyRepository{groups: map[uint64]domain.Group{
		1: {ID: 1, Scope: domain.ScopeWeb, Enabled: true},
	}}
	if _, err := resolveRegistrationProxyGroup(context.Background(), repo, cipher, 1, "grok_build", make(map[uint64]struct{})); err == nil {
		t.Fatal("scope mismatch was accepted")
	}
}

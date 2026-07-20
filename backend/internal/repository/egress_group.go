package repository

import (
	"context"
	"github.com/owen891/grok2api/backend/internal/domain/egress"
)

type EgressGroupRepository interface {
	ListEgressGroups(ctx context.Context, scope egress.Scope) ([]egress.Group, error)
	GetEgressGroup(ctx context.Context, id uint64) (egress.Group, error)
	CreateEgressGroup(ctx context.Context, value egress.Group) (egress.Group, error)
	UpdateEgressGroup(ctx context.Context, value egress.Group) (egress.Group, error)
	DeleteEgressGroup(ctx context.Context, id uint64) error
	ListEgressGroupMembers(ctx context.Context, groupID uint64) ([]egress.GroupMember, error)
	UpsertEgressGroupMember(ctx context.Context, value egress.GroupMember) (egress.GroupMember, error)
	DeleteEgressGroupMember(ctx context.Context, groupID, nodeID uint64) error
}

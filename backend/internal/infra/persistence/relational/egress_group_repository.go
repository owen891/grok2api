package relational

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func (r *EgressRepository) ListEgressGroups(ctx context.Context, scope egress.Scope) ([]egress.Group, error) {
	query := r.db.db.WithContext(ctx).Model(&egressGroupModel{}).Order("scope ASC, LOWER(name) ASC, id ASC")
	if scope != "" {
		query = query.Where("scope = ?", scope)
	}
	var rows []egressGroupModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]egress.Group, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressGroupDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) GetEgressGroup(ctx context.Context, id uint64) (egress.Group, error) {
	var row egressGroupModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return egress.Group{}, mapError(err)
	}
	return toEgressGroupDomain(row), nil
}

func (r *EgressRepository) CreateEgressGroup(ctx context.Context, value egress.Group) (egress.Group, error) {
	row := fromEgressGroupDomain(value)
	if err := r.db.db.WithContext(ctx).Create(&row).Error; err != nil {
		return egress.Group{}, mapError(err)
	}
	return toEgressGroupDomain(row), nil
}

func (r *EgressRepository) UpdateEgressGroup(ctx context.Context, value egress.Group) (egress.Group, error) {
	row := fromEgressGroupDomain(value)
	result := r.db.db.WithContext(ctx).Save(&row)
	if result.Error != nil {
		return egress.Group{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.Group{}, repository.ErrNotFound
	}
	return toEgressGroupDomain(row), nil
}

func (r *EgressRepository) DeleteEgressGroup(ctx context.Context, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&egressGroupModel{}).Where("fallback_group_id = ?", id).Update("fallback_group_id", nil).Error; err != nil {
			return mapError(err)
		}
		// Model routes store the group ID as a scalar so older databases can
		// migrate without a foreign key. Detach those routes before deleting
		// the group instead of leaving every matching request broken.
		if err := tx.Model(&modelRouteModel{}).Where("egress_group_id = ?", id).Update("egress_group_id", 0).Error; err != nil {
			return mapError(err)
		}
		if err := tx.Delete(&egressGroupMemberModel{}, "group_id = ?", id).Error; err != nil {
			return mapError(err)
		}
		result := tx.Delete(&egressGroupModel{}, id)
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
}

func (r *EgressRepository) ListEgressGroupMembers(ctx context.Context, groupID uint64) ([]egress.GroupMember, error) {
	var rows []egressGroupMemberModel
	if err := r.db.db.WithContext(ctx).Where("group_id = ?", groupID).Order("priority DESC, node_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]egress.GroupMember, 0, len(rows))
	for _, row := range rows {
		values = append(values, toEgressGroupMemberDomain(row))
	}
	return values, nil
}

func (r *EgressRepository) UpsertEgressGroupMember(ctx context.Context, value egress.GroupMember) (egress.GroupMember, error) {
	row := fromEgressGroupMemberDomain(value)
	if err := r.db.db.WithContext(ctx).Save(&row).Error; err != nil {
		return egress.GroupMember{}, mapError(err)
	}
	return toEgressGroupMemberDomain(row), nil
}

func (r *EgressRepository) DeleteEgressGroupMember(ctx context.Context, groupID, nodeID uint64) error {
	result := r.db.db.WithContext(ctx).Delete(&egressGroupMemberModel{}, "group_id = ? AND node_id = ?", groupID, nodeID)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

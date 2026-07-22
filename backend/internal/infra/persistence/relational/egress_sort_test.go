package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/owen891/grok2api/backend/internal/domain/egress"
	"github.com/owen891/grok2api/backend/internal/repository"
)

type legacyAllEgressNode struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement"`
	Scope string `gorm:"size:32;not null"`
}

func (legacyAllEgressNode) TableName() string { return "egress_nodes" }

func TestEgressRepositorySortsInDatabase(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-sort.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(database)
	for _, value := range []egress.Node{
		{Name: "slow", Scope: egress.ScopeBuild, Enabled: true, ProxyPool: true, Health: 0.2},
		{Name: "healthy", Scope: egress.ScopeWeb, Enabled: true, Health: 0.9},
		{Name: "middle", Scope: egress.ScopeWebAsset, Enabled: true, Health: 0.5},
	} {
		if _, err := repo.CreateEgressNode(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := repo.ListEgressNodes(ctx, "", repository.SortQuery{Field: "health", Direction: repository.SortDescending})
	if err != nil || len(values) != 3 || values[0].Name != "healthy" || values[2].Name != "slow" {
		t.Fatalf("health sort = %#v, err = %v", values, err)
	}
	var slow egress.Node
	for _, value := range values {
		if value.Name == "slow" {
			slow = value
		}
	}
	if !slow.ProxyPool {
		t.Fatal("proxy pool flag was not persisted")
	}
}

func TestInitializeSchemaRemovesAndRejectsLegacyAllEgressNodes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.WithContext(ctx).AutoMigrate(&legacyAllEgressNode{}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Create(&legacyAllEgressNode{Scope: "all"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := database.db.WithContext(ctx).Model(&egressNodeModel{}).Where("scope = ?", "all").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy all-scope nodes = %d", count)
	}
	if _, err := NewEgressRepository(database).CreateEgressNode(ctx, egress.Node{Name: "invalid", Scope: egress.Scope("all"), Enabled: true}); err == nil {
		t.Fatal("all-scope node passed the database constraint")
	}
}

func TestEgressRelationsAreCleanedWhenGroupsOrNodesAreDeleted(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	groups := NewEgressRepository(database)
	node, err := nodes.CreateEgressNode(ctx, egress.Node{Name: "member", Scope: egress.ScopeBuild, Enabled: true, Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	fallback := uint64(2)
	if _, err := groups.CreateEgressGroup(ctx, egress.Group{ID: 1, Name: "primary", Scope: egress.ScopeBuild, Enabled: true, FallbackGroupID: &fallback}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.CreateEgressGroup(ctx, egress.Group{ID: 2, Name: "fallback", Scope: egress.ScopeBuild, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.UpsertEgressGroupMember(ctx, egress.GroupMember{GroupID: 1, NodeID: node.ID, Enabled: true, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := groups.DeleteEgressGroup(ctx, 2); err != nil {
		t.Fatal(err)
	}
	primary, err := groups.GetEgressGroup(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if primary.FallbackGroupID != nil {
		t.Fatalf("fallback reference survived delete: %#v", primary.FallbackGroupID)
	}
	if err := nodes.DeleteEgressNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	members, err := groups.ListEgressGroupMembers(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("node membership survived delete: %#v", members)
	}
}

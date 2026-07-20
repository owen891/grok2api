package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	egressdomain "github.com/owen891/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
)

func TestDeleteEgressGroupDetachesModelRoutes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress-group.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	groupRepo := NewEgressRepository(database)
	group, err := groupRepo.CreateEgressGroup(ctx, egressdomain.Group{Name: "primary", Scope: egressdomain.ScopeBuild, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	routeRepo := NewModelRepository(database)
	route, err := routeRepo.Create(ctx, modeldomain.Route{PublicID: "grok-test", Provider: account.ProviderBuild, UpstreamModel: "grok-test", Capability: modeldomain.CapabilityResponses, Enabled: true, EgressGroupID: group.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := groupRepo.DeleteEgressGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := routeRepo.Get(ctx, route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EgressGroupID != 0 {
		t.Fatalf("route egress group = %d, want detached", updated.EgressGroupID)
	}
}

package egressgroup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	app "github.com/chenyme/grok2api/backend/internal/application/egressgroup"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type handlerGroupRepository struct {
	group   domain.Group
	groups  map[uint64]domain.Group
	nodes   []domain.Node
	members []domain.GroupMember
}

func (r *handlerGroupRepository) ListEgressGroups(_ context.Context, scope domain.Scope) ([]domain.Group, error) {
	values := make([]domain.Group, 0, len(r.groups))
	for _, value := range r.groups {
		if scope == "" || value.Scope == scope {
			values = append(values, value)
		}
	}
	return values, nil
}
func (r *handlerGroupRepository) GetEgressGroup(_ context.Context, id uint64) (domain.Group, error) {
	value, ok := r.groups[id]
	if !ok {
		return domain.Group{}, repository.ErrNotFound
	}
	return value, nil
}
func (r *handlerGroupRepository) CreateEgressGroup(_ context.Context, value domain.Group) (domain.Group, error) {
	value.ID = 1
	r.groups[value.ID] = value
	return value, nil
}
func (r *handlerGroupRepository) UpdateEgressGroup(_ context.Context, value domain.Group) (domain.Group, error) {
	r.groups[value.ID] = value
	return value, nil
}
func (r *handlerGroupRepository) DeleteEgressGroup(_ context.Context, id uint64) error {
	if _, ok := r.groups[id]; !ok {
		return repository.ErrNotFound
	}
	delete(r.groups, id)
	return nil
}
func (r *handlerGroupRepository) ListEgressGroupMembers(context.Context, uint64) ([]domain.GroupMember, error) {
	return r.members, nil
}
func (r *handlerGroupRepository) UpsertEgressGroupMember(_ context.Context, value domain.GroupMember) (domain.GroupMember, error) {
	r.members = append(r.members, value)
	return value, nil
}
func (r *handlerGroupRepository) DeleteEgressGroupMember(_ context.Context, groupID, nodeID uint64) error {
	for index, value := range r.members {
		if value.GroupID == groupID && value.NodeID == nodeID {
			r.members = append(r.members[:index], r.members[index+1:]...)
			return nil
		}
	}
	return repository.ErrNotFound
}
func (r *handlerGroupRepository) ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error) {
	return r.nodes, nil
}
func (r *handlerGroupRepository) CreateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	value.ID = uint64(len(r.nodes) + 1)
	r.nodes = append(r.nodes, value)
	return value, nil
}

func TestHandlerGroupCRUDAndImportContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &handlerGroupRepository{groups: make(map[uint64]domain.Group)}
	router := gin.New()
	NewHandler(app.NewService(repo, repo, nil)).Register(router.Group("/api/admin/v1"))

	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/egress-groups", strings.NewReader(`{"name":"primary","scope":"grok_build","enabled":true,"strategy":"round_robin","maxConcurrency":2}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(create, request)
	if create.Code != http.StatusCreated || !strings.Contains(create.Body.String(), `"name":"primary"`) {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}

	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/admin/v1/egress-groups", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"items"`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}

	importResponse := httptest.NewRecorder()
	importRequest := httptest.NewRequest(http.MethodPost, "/api/admin/v1/egress-groups/1/import", strings.NewReader(`{"content":"127.0.0.1:8080\nhttp://127.0.0.2:8080","dryRun":true}`))
	importRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(importResponse, importRequest)
	if importResponse.Code != http.StatusOK || !strings.Contains(importResponse.Body.String(), `"items"`) || strings.Count(importResponse.Body.String(), `"line"`) != 2 {
		t.Fatalf("import status=%d body=%s", importResponse.Code, importResponse.Body.String())
	}

	remove := httptest.NewRecorder()
	router.ServeHTTP(remove, httptest.NewRequest(http.MethodDelete, "/api/admin/v1/egress-groups/1", nil))
	if remove.Code != http.StatusOK || !strings.Contains(remove.Body.String(), `"deleted":true`) {
		t.Fatalf("delete status=%d body=%s", remove.Code, remove.Body.String())
	}
}

func TestHandlerRejectsInvalidScopeAndStrategy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &handlerGroupRepository{groups: make(map[uint64]domain.Group)}
	router := gin.New()
	NewHandler(app.NewService(repo, repo, nil)).Register(router.Group("/api/admin/v1"))
	for _, body := range []string{
		`{"name":"bad-scope","scope":"all","enabled":true}`,
		`{"name":"bad-strategy","scope":"grok_build","enabled":true,"strategy":"random"}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/egress-groups", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"invalidEgressGroup"`) {
			t.Fatalf("body=%s status=%d", recorder.Body.String(), recorder.Code)
		}
	}
}

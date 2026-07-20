package egressgroup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	egressapp "github.com/owen891/grok2api/backend/internal/application/egress"
	domain "github.com/owen891/grok2api/backend/internal/domain/egress"
	"github.com/owen891/grok2api/backend/internal/infra/security"
	"github.com/owen891/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput = errors.New("代理组参数无效")
	ErrNotFound     = errors.New("代理组不存在")
)

type Input struct {
	Name            string
	Scope           domain.Scope
	Enabled         bool
	Strategy        domain.GroupStrategy
	MaxConcurrency  int
	FallbackGroupID *uint64
}

type MemberInput struct {
	NodeID         uint64
	Weight         int
	MaxConcurrency int
	Enabled        bool
	Priority       int
}

type ImportLine struct {
	Line  int    `json:"line"`
	Value string `json:"value"`
	Name  string `json:"name,omitempty"`
}

type ImportResult struct {
	Line            int    `json:"line"`
	ProxyConfigured bool   `json:"proxyConfigured"`
	NodeID          uint64 `json:"nodeId,string,omitempty"`
	Created         bool   `json:"created"`
	Reused          bool   `json:"reused"`
	Error           string `json:"error,omitempty"`
}

type Service struct {
	repository repository.EgressGroupRepository
	nodes      interface {
		ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error)
		CreateEgressNode(context.Context, domain.Node) (domain.Node, error)
	}
	cipher *security.Cipher
}

func NewService(groups repository.EgressGroupRepository, nodes interface {
	ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error)
	CreateEgressNode(context.Context, domain.Node) (domain.Node, error)
}, cipher *security.Cipher) *Service {
	return &Service{repository: groups, nodes: nodes, cipher: cipher}
}

func (s *Service) List(ctx context.Context, scope domain.Scope) ([]domain.Group, error) {
	return s.repository.ListEgressGroups(ctx, scope)
}

func (s *Service) Create(ctx context.Context, input Input) (domain.Group, error) {
	value, err := normalizeInput(input)
	if err != nil {
		return domain.Group{}, err
	}
	if err := s.validateFallback(ctx, 0, value); err != nil {
		return domain.Group{}, err
	}
	created, err := s.repository.CreateEgressGroup(ctx, value)
	if err != nil {
		return domain.Group{}, err
	}
	return created, nil
}

func (s *Service) Update(ctx context.Context, id uint64, input Input) (domain.Group, error) {
	value, err := s.repository.GetEgressGroup(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Group{}, ErrNotFound
	}
	if err != nil {
		return domain.Group{}, err
	}
	next, err := normalizeInput(input)
	if err != nil {
		return domain.Group{}, err
	}
	next.ID, next.CreatedAt = value.ID, value.CreatedAt
	if err := s.validateFallback(ctx, id, next); err != nil {
		return domain.Group{}, err
	}
	updated, err := s.repository.UpdateEgressGroup(ctx, next)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Group{}, ErrNotFound
	}
	return updated, err
}

func (s *Service) validateFallback(ctx context.Context, currentID uint64, value domain.Group) error {
	nextID := value.FallbackGroupID
	if nextID == nil {
		return nil
	}
	seen := make(map[uint64]struct{})
	if currentID != 0 {
		seen[currentID] = struct{}{}
	}
	for nextID != nil {
		if *nextID == 0 {
			return fmt.Errorf("%w: fallback group ID is invalid", ErrInvalidInput)
		}
		if _, exists := seen[*nextID]; exists {
			return fmt.Errorf("%w: fallback group cycle detected", ErrInvalidInput)
		}
		seen[*nextID] = struct{}{}
		fallback, err := s.repository.GetEgressGroup(ctx, *nextID)
		if errors.Is(err, repository.ErrNotFound) {
			return fmt.Errorf("%w: fallback group does not exist", ErrInvalidInput)
		}
		if err != nil {
			return err
		}
		if fallback.Scope != value.Scope {
			return fmt.Errorf("%w: fallback group scope does not match", ErrInvalidInput)
		}
		nextID = fallback.FallbackGroupID
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressGroup(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) Members(ctx context.Context, groupID uint64) ([]domain.GroupMember, error) {
	return s.repository.ListEgressGroupMembers(ctx, groupID)
}

func (s *Service) UpsertMember(ctx context.Context, groupID uint64, input MemberInput) (domain.GroupMember, error) {
	if groupID == 0 || input.NodeID == 0 || input.Weight < 0 || input.Weight > 10000 || input.MaxConcurrency < 0 {
		return domain.GroupMember{}, ErrInvalidInput
	}
	group, err := s.repository.GetEgressGroup(ctx, groupID)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.GroupMember{}, ErrNotFound
	}
	if err != nil {
		return domain.GroupMember{}, err
	}
	nodes, err := s.nodes.ListEgressNodes(ctx, group.Scope, repository.SortQuery{})
	if err != nil {
		return domain.GroupMember{}, err
	}
	found := false
	for _, node := range nodes {
		if node.ID == input.NodeID {
			found = true
			break
		}
	}
	if !found {
		return domain.GroupMember{}, fmt.Errorf("%w: 节点不属于组 scope", ErrInvalidInput)
	}
	if input.Weight == 0 {
		input.Weight = 1
	}
	return s.repository.UpsertEgressGroupMember(ctx, domain.GroupMember{GroupID: groupID, NodeID: input.NodeID, Weight: input.Weight, MaxConcurrency: input.MaxConcurrency, Enabled: input.Enabled, Priority: input.Priority})
}

func (s *Service) DeleteMember(ctx context.Context, groupID, nodeID uint64) error {
	return s.repository.DeleteEgressGroupMember(ctx, groupID, nodeID)
}

func (s *Service) Import(ctx context.Context, groupID uint64, lines []ImportLine, dryRun bool, defaults MemberInput) ([]ImportResult, error) {
	group, err := s.repository.GetEgressGroup(ctx, groupID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(lines) > 5000 {
		return nil, fmt.Errorf("%w: 单次最多导入 5000 行", ErrInvalidInput)
	}
	results := make([]ImportResult, 0, len(lines))
	existing, err := s.nodes.ListEgressNodes(ctx, group.Scope, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	byProxy := make(map[string]domain.Node)
	for _, node := range existing {
		if node.EncryptedProxyURL == "" {
			continue
		}
		if s.cipher == nil {
			continue
		}
		plain, decryptErr := s.cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr == nil {
			byProxy[plain] = node
		}
	}
	for _, line := range lines {
		value := strings.TrimSpace(line.Value)
		result := ImportResult{Line: line.Line, ProxyConfigured: value != ""}
		if value == "" {
			result.Error = "代理地址为空"
			results = append(results, result)
			continue
		}
		normalized, normalizeErr := normalizeImportProxy(value)
		if normalizeErr != nil {
			result.Error = normalizeErr.Error()
			results = append(results, result)
			continue
		}
		if node, ok := byProxy[normalized]; ok {
			result.NodeID, result.Reused = node.ID, true
			if !dryRun {
				_, err = s.repository.UpsertEgressGroupMember(ctx, domain.GroupMember{GroupID: groupID, NodeID: node.ID, Weight: max(1, defaults.Weight), MaxConcurrency: defaults.MaxConcurrency, Enabled: defaults.Enabled, Priority: defaults.Priority})
			}
			if err != nil {
				result.Error = err.Error()
			}
			results = append(results, result)
			continue
		}
		if dryRun {
			results = append(results, result)
			continue
		}
		name := strings.TrimSpace(line.Name)
		if name == "" {
			name = "proxy-" + strconv.Itoa(line.Line)
		}
		node, createErr := s.createNode(ctx, group.Scope, name, normalized)
		if createErr != nil {
			result.Error = createErr.Error()
			results = append(results, result)
			continue
		}
		byProxy[normalized] = node
		result.NodeID, result.Created = node.ID, true
		_, err = s.repository.UpsertEgressGroupMember(ctx, domain.GroupMember{GroupID: groupID, NodeID: node.ID, Weight: max(1, defaults.Weight), MaxConcurrency: defaults.MaxConcurrency, Enabled: defaults.Enabled, Priority: defaults.Priority})
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results, nil
}

func normalizeImportProxy(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return egressapp.NormalizeProxyURL(value)
}

func (s *Service) createNode(ctx context.Context, scope domain.Scope, name, proxy string) (domain.Node, error) {
	if s.cipher == nil {
		return domain.Node{}, errors.New("代理加密器未初始化")
	}
	encrypted, err := s.cipher.Encrypt(proxy)
	if err != nil {
		return domain.Node{}, err
	}
	return s.nodes.CreateEgressNode(ctx, domain.Node{Name: name, Scope: scope, Enabled: true, EncryptedProxyURL: encrypted, Health: 1})
}

func normalizeInput(input Input) (domain.Group, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.Group{}, fmt.Errorf("%w: 名称长度必须为 1-160", ErrInvalidInput)
	}
	if input.Scope != domain.ScopeBuild && input.Scope != domain.ScopeWeb && input.Scope != domain.ScopeConsole && input.Scope != domain.ScopeWebAsset {
		return domain.Group{}, fmt.Errorf("%w: scope 无效", ErrInvalidInput)
	}
	if input.Strategy == "" {
		input.Strategy = domain.StrategyLeastLoad
	}
	if input.Strategy != domain.StrategyLeastLoad && input.Strategy != domain.StrategyWeighted && input.Strategy != domain.StrategySticky && input.Strategy != domain.StrategyRoundRobin {
		return domain.Group{}, fmt.Errorf("%w: 策略无效", ErrInvalidInput)
	}
	if input.MaxConcurrency < 0 || input.MaxConcurrency > 100000 {
		return domain.Group{}, fmt.Errorf("%w: 并发限制无效", ErrInvalidInput)
	}
	return domain.Group{Name: name, Scope: input.Scope, Enabled: input.Enabled, Strategy: input.Strategy, MaxConcurrency: input.MaxConcurrency, FallbackGroupID: input.FallbackGroupID}, nil
}

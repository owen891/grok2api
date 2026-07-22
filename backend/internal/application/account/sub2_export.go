package account

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	accountdomain "github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/repository"
)

const (
	sub2DataType     = "sub2api-data"
	sub2DataVersion  = 1
	sub2Platform     = "grok"
	sub2AccountType  = "oauth"
	sub2ClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	sub2Scope        = "openid profile email offline_access grok-cli:access api:access"
	sub2BaseURL      = "https://cli-chat-proxy.grok.com/v1"
	sub2PriorityBase = 50
)

type sub2ExportDocument struct {
	Type       string              `json:"type"`
	Version    int                 `json:"version"`
	ExportedAt string              `json:"exported_at"`
	Proxies    []any               `json:"proxies"`
	Accounts   []sub2ExportAccount `json:"accounts"`
}

type sub2ExportAccount struct {
	Name               string                `json:"name"`
	Platform           string                `json:"platform"`
	Type               string                `json:"type"`
	Credentials        sub2ExportCredentials `json:"credentials"`
	Concurrency        int                   `json:"concurrency"`
	Priority           int                   `json:"priority"`
	RateMultiplier     int                   `json:"rate_multiplier"`
	AutoPauseOnExpired bool                  `json:"auto_pause_on_expired"`
	ExpiresAt          int64                 `json:"expires_at,omitempty"`
}

type sub2ExportCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Email        string `json:"email,omitempty"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
	BaseURL      string `json:"base_url"`
}

type sub2ExportSource struct {
	credential   accountdomain.Credential
	accessToken  string
	refreshToken string
}

// ExportSub2Credentials exports refreshable Grok Build OAuth accounts in Sub2API's data import format.
func (s *Service) ExportSub2Credentials(ctx context.Context) (ExportResult, error) {
	now := s.now()
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page:   repository.PageQuery{Limit: maxCredentialExportAccounts + 1},
		Filter: repository.AccountListFilter{Provider: string(accountdomain.ProviderBuild), Now: now},
	})
	if err != nil {
		return ExportResult{}, err
	}
	if total > maxCredentialExportAccounts {
		return ExportResult{}, fmt.Errorf("%w: 单次最多导出 10000 个账号", ErrExportLimit)
	}

	sources := make([]sub2ExportSource, 0, len(values))
	skipped := 0
	for _, value := range values {
		if value.Provider != accountdomain.ProviderBuild || value.EncryptedAccessToken == "" || value.EncryptedRefreshToken == "" {
			skipped++
			continue
		}
		accessToken, err := s.cipher.Decrypt(value.EncryptedAccessToken)
		if err != nil {
			return ExportResult{}, fmt.Errorf("解密账号 %d access token: %w", value.ID, err)
		}
		refreshToken, err := s.cipher.Decrypt(value.EncryptedRefreshToken)
		if err != nil {
			return ExportResult{}, fmt.Errorf("解密账号 %d refresh token: %w", value.ID, err)
		}
		if strings.TrimSpace(accessToken) == "" || strings.TrimSpace(refreshToken) == "" {
			skipped++
			continue
		}
		sources = append(sources, sub2ExportSource{credential: value, accessToken: accessToken, refreshToken: refreshToken})
	}
	if len(sources) == 0 {
		return ExportResult{}, invalidInput("没有同时包含 access token 和 refresh token 的 Grok Build 账号")
	}
	priorityBySource := sub2PriorityMap(sources)
	accounts := make([]sub2ExportAccount, 0, len(sources))
	for _, source := range sources {
		accounts = append(accounts, newSub2ExportAccount(source.credential, source.accessToken, source.refreshToken, priorityBySource[source.credential.Priority]))
	}

	document := sub2ExportDocument{
		Type:       sub2DataType,
		Version:    sub2DataVersion,
		ExportedAt: now.UTC().Format(time.RFC3339),
		Proxies:    []any{},
		Accounts:   accounts,
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return ExportResult{}, err
	}
	data = append(data, '\n')
	return ExportResult{Data: data, Count: len(accounts), Skipped: skipped}, nil
}

// sub2PriorityMap preserves source priority tiers while converting from descending to ascending priority semantics.
func sub2PriorityMap(sources []sub2ExportSource) map[int]int {
	levels := make(map[int]struct{}, len(sources))
	for _, source := range sources {
		levels[source.credential.Priority] = struct{}{}
	}
	sortedLevels := make([]int, 0, len(levels))
	for priority := range levels {
		sortedLevels = append(sortedLevels, priority)
	}
	sort.Slice(sortedLevels, func(i, j int) bool { return sortedLevels[i] > sortedLevels[j] })

	priorityBySource := make(map[int]int, len(sortedLevels))
	for index, priority := range sortedLevels {
		priorityBySource[priority] = sub2PriorityBase + index
	}
	return priorityBySource
}

func newSub2ExportAccount(value accountdomain.Credential, accessToken, refreshToken string, priority int) sub2ExportAccount {
	name := strings.TrimSpace(value.Name)
	if name == "" {
		name = strings.TrimSpace(value.Email)
	}
	if name == "" {
		name = "Grok OAuth Account"
	}
	clientID := strings.TrimSpace(value.OIDCClientID)
	if clientID == "" {
		clientID = sub2ClientID
	}
	concurrency := value.MaxConcurrent
	if concurrency < 1 {
		concurrency = 1
	}

	account := sub2ExportAccount{
		Name:     name,
		Platform: sub2Platform,
		Type:     sub2AccountType,
		Credentials: sub2ExportCredentials{
			AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer",
			Email: strings.TrimSpace(value.Email), ClientID: clientID, Scope: sub2Scope, BaseURL: sub2BaseURL,
		},
		Concurrency: concurrency, Priority: priority, RateMultiplier: 1, AutoPauseOnExpired: false,
	}
	if !value.ExpiresAt.IsZero() {
		account.Credentials.ExpiresAt = value.ExpiresAt.UTC().Format(time.RFC3339)
		account.ExpiresAt = value.ExpiresAt.Unix()
	}
	return account
}

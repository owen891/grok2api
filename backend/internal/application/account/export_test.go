package account

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/owen891/grok2api/backend/internal/infra/provider/cli"
	"github.com/owen891/grok2api/backend/internal/infra/security"
)

func TestExportCredentialsRoundTripsImportFormat(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := cipher.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	repository := relational.NewAccountRepository(database)
	if _, _, err := repository.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "primary", Email: "user@example.com", UserID: "user-1",
		SourceKey: "export-test", OIDCClientID: "client-1", EncryptedAccessToken: accessToken,
		EncryptedRefreshToken: refreshToken, ExpiresAt: expiresAt, Enabled: false,
		AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 8,
	}); err != nil {
		t.Fatal(err)
	}
	adapter := cliprovider.NewAdapter(cliprovider.Config{}, cipher)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)

	result, err := service.ExportCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	values, err := adapter.ParseImportedCredentials(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || len(values) != 1 {
		t.Fatalf("export count = %d, imported values = %d", result.Count, len(values))
	}
	value := values[0]
	if value.Name != "primary" || value.Email != "user@example.com" || value.UserID != "user-1" || value.OIDCClientID != "client-1" || value.AccessToken != "access-token" || value.RefreshToken != "refresh-token" || !value.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("round-trip credential = %#v", value)
	}
	progress := make([][2]int, 0, 2)
	if _, err := service.ImportCredentialsWithProgress(ctx, result.Data, nil, func(completed, total int) error {
		progress = append(progress, [2]int{completed, total})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 || progress[0] != [2]int{0, 1} || progress[1] != [2]int{1, 1} {
		t.Fatalf("import progress = %#v", progress)
	}

	multiProgress := make([][2]int, 0, 3)
	multiResult, err := service.ImportCredentialDocumentsWithProgress(ctx, [][]byte{
		result.Data,
		result.Data,
		[]byte(`{"provider":"grok_build","name":"secondary","access_token":"second-access","refresh_token":"second-refresh","user_id":"user-2"}`),
	}, nil, func(completed, total int) error {
		multiProgress = append(multiProgress, [2]int{completed, total})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if multiResult.Created != 1 || multiResult.Updated != 1 {
		t.Fatalf("multi-file import result = %#v", multiResult)
	}
	if len(multiProgress) != 3 || multiProgress[0] != [2]int{0, 2} || multiProgress[2] != [2]int{2, 2} {
		t.Fatalf("multi-file import progress = %#v", multiProgress)
	}
}

func TestExportSub2CredentialsMatchesImportContract(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sub2-export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := cipher.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	exportedAt := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	repository := relational.NewAccountRepository(database)
	for _, value := range []accountdomain.Credential{
		{
			Provider: accountdomain.ProviderBuild, Name: "primary", Email: "user@example.com", SourceKey: "sub2-valid",
			OIDCClientID: "client-1", EncryptedAccessToken: accessToken, EncryptedRefreshToken: refreshToken,
			ExpiresAt: expiresAt, Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 7, MaxConcurrent: 12,
		},
		{
			Provider: accountdomain.ProviderBuild, Name: "not-refreshable", SourceKey: "sub2-no-refresh",
			EncryptedAccessToken: accessToken, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
			Priority: 1, MaxConcurrent: 8,
		},
	} {
		if _, _, err := repository.UpsertByIdentity(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(), cipher, nil)
	service.now = func() time.Time { return exportedAt }

	result, err := service.ExportSub2Credentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "type": "sub2api-data",
  "version": 1,
  "exported_at": "2026-07-21T09:30:00Z",
  "proxies": [],
  "accounts": [
    {
      "name": "primary",
      "platform": "grok",
      "type": "oauth",
      "credentials": {
        "access_token": "access-token",
        "refresh_token": "refresh-token",
        "token_type": "Bearer",
        "expires_at": "2026-07-12T12:00:00Z",
        "email": "user@example.com",
        "client_id": "client-1",
        "scope": "openid profile email offline_access grok-cli:access api:access",
        "base_url": "https://cli-chat-proxy.grok.com/v1"
      },
      "concurrency": 12,
      "priority": 50,
      "rate_multiplier": 1,
      "auto_pause_on_expired": false,
      "expires_at": 1783857600
    }
  ]
}
`
	if result.Count != 1 || result.Skipped != 1 || string(result.Data) != want {
		t.Fatalf("export result = %#v, document = %s", result, result.Data)
	}
}

func TestSub2PriorityMapReversesPriorityOrderByTier(t *testing.T) {
	priorities := sub2PriorityMap([]sub2ExportSource{
		{credential: accountdomain.Credential{Priority: 500}},
		{credential: accountdomain.Credential{Priority: 100}},
		{credential: accountdomain.Credential{Priority: 500}},
		{credential: accountdomain.Credential{Priority: -1}},
	})
	if priorities[500] != 50 || priorities[100] != 51 || priorities[-1] != 52 {
		t.Fatalf("priority mapping = %#v", priorities)
	}
}

func TestExportSub2CredentialsRejectsEmptyRefreshableSet(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "sub2-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAccountRepository(database)
	service := NewService(repository, nil, nil, nil, provider.NewRegistry(), cipher, nil)

	_, err = service.ExportSub2Credentials(ctx)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
}

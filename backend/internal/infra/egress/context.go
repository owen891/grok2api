package egress

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type groupContextKey struct{}

type accountContextKey struct{}

func WithGroupID(ctx context.Context, groupID uint64) context.Context {
	if groupID == 0 {
		return ctx
	}
	return context.WithValue(ctx, groupContextKey{}, groupID)
}

func WithAccount(ctx context.Context, provider string, accountID uint64) context.Context {
	if accountID == 0 {
		return ctx
	}
	return context.WithValue(ctx, accountContextKey{}, struct {
		Provider string
		ID       uint64
	}{Provider: provider, ID: accountID})
}

func WithCredential(ctx context.Context, credential account.Credential) context.Context {
	return WithAccount(ctx, string(credential.Provider), credential.ID)
}

func groupIDFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	value, _ := ctx.Value(groupContextKey{}).(uint64)
	return value
}

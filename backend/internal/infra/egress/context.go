package egress

import "context"

type groupContextKey struct{}

func WithGroupID(ctx context.Context, groupID uint64) context.Context {
	if groupID == 0 {
		return ctx
	}
	return context.WithValue(ctx, groupContextKey{}, groupID)
}

func groupIDFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	value, _ := ctx.Value(groupContextKey{}).(uint64)
	return value
}

// Package tenant carries the resolved tenant identity through the request context.
package tenant

import "context"

type ctxKey struct{}

// WithID stores the tenant id in the context.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the tenant id from the context, or empty if none is set.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

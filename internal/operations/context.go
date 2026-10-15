package operations

import "context"

type ctxKey struct{}

// WithContext returns a context carrying op so the execution lanes and the
// audit logger can discover the current operation without an explicit
// parameter through every helper.
func WithContext(ctx context.Context, op *Operation) context.Context {
	return context.WithValue(ctx, ctxKey{}, op)
}

// FromContext returns the operation attached to ctx, or nil when the request
// path did not begin one.
func FromContext(ctx context.Context) *Operation {
	op, _ := ctx.Value(ctxKey{}).(*Operation)
	return op
}

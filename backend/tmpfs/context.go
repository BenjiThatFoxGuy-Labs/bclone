package tmpfs

import "context"

type dockerContextKey struct{}

// WithDockerContext marks ctx as coming from the Docker volume plugin.
func WithDockerContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, dockerContextKey{}, true)
}

func isDockerContext(ctx context.Context) bool {
	v, _ := ctx.Value(dockerContextKey{}).(bool)
	return v
}

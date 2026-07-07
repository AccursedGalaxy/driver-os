package agent

import (
	"context"
	"testing"
	"time"
)

func mustNewGates(t *testing.T, ctx context.Context, cfg Config, rt time.Duration) *gates {
	t.Helper()
	g, err := newGates(ctx, cfg, rt)
	if err != nil {
		t.Fatalf("newGates: %v", err)
	}
	return g
}

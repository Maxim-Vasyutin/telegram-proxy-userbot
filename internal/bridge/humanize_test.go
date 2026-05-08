package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHumanizer_WaitInRange(t *testing.T) {
	const minMs, maxMs = 50, 150
	h := NewHumanizer(minMs, maxMs)

	start := time.Now()
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsedMs := time.Since(start).Milliseconds()

	// Allow generous tolerance for CI scheduling jitter.
	const tol = 100
	if elapsedMs < int64(minMs-tol) || elapsedMs > int64(maxMs+tol) {
		t.Fatalf("elapsed %dms not in tolerant range [%d, %d]ms",
			elapsedMs, minMs-tol, maxMs+tol)
	}
}

func TestHumanizer_CancelledContext(t *testing.T) {
	h := NewHumanizer(10_000, 20_000) // very long delay
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Wait

	err := h.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestHumanizer_ZeroDelay(t *testing.T) {
	h := NewHumanizer(0, 0)
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("unexpected error on zero-delay humanizer: %v", err)
	}
}

func TestHumanizer_NilSafe(t *testing.T) {
	var h *Humanizer
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("nil Humanizer.Wait should be a no-op, got: %v", err)
	}
}

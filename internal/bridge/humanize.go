package bridge

import (
	"context"
	"math/rand/v2"
	"time"
)

// Humanizer injects a random delay before outgoing relay messages so the
// userbot behaves less like a machine. Applied only before new-message sends
// (not before edit, delete, or reaction calls — those feel natural when fast).
type Humanizer struct {
	minMs int
	maxMs int
}

// NewHumanizer constructs a Humanizer that sleeps between minMs and maxMs
// milliseconds on each Wait call. If maxMs < minMs it is silently clamped to
// minMs (zero delay range = constant delay).
func NewHumanizer(minMs, maxMs int) *Humanizer {
	if maxMs < minMs {
		maxMs = minMs
	}
	return &Humanizer{minMs: minMs, maxMs: maxMs}
}

// Wait blocks for a uniformly random duration in [minMs, maxMs] milliseconds.
// Returns ctx.Err() immediately if the context is already cancelled.
func (h *Humanizer) Wait(ctx context.Context) error {
	if h == nil || (h.minMs == 0 && h.maxMs == 0) {
		return nil
	}

	delta := h.maxMs - h.minMs
	var ms int
	if delta > 0 {
		ms = h.minMs + rand.IntN(delta+1)
	} else {
		ms = h.minMs
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return nil
	}
}

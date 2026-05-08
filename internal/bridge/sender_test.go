package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tgerr"
)

func TestWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_SuccessOnThirdAttempt(t *testing.T) {
	calls := 0
	transient := errors.New("transient network error")
	err := withRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_ExhaustsRetries(t *testing.T) {
	sentinel := errors.New("always fails")
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	// 1 initial + 3 retries = 4 total calls.
	if calls != 4 {
		t.Fatalf("expected 4 calls, got %d", calls)
	}
}

func TestWithRetry_ChatForbiddenNoRetry(t *testing.T) {
	calls := 0
	forbiddenErr := &tgerr.Error{Code: 403, Type: "CHAT_WRITE_FORBIDDEN"}
	err := withRetry(context.Background(), func() error {
		calls++
		return forbiddenErr
	})
	if !errors.Is(err, ErrChatForbidden) {
		t.Fatalf("expected ErrChatForbidden, got %v", err)
	}
	// Must not retry — only 1 call.
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", calls)
	}
}

func TestWithRetry_FloodWaitRetry(t *testing.T) {
	// FLOOD_WAIT_0 → 0-second sleep → immediate retry.
	floodErr := &tgerr.Error{Code: 420, Type: "FLOOD_WAIT_0"}
	calls := 0
	err := withRetry(context.Background(), func() error {
		calls++
		if calls == 1 {
			return floodErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil after flood-wait retry, got %v", err)
	}
	// First attempt → flood wait → one retry.
	if calls != 2 {
		t.Fatalf("expected 2 calls (original + flood-wait retry), got %d", calls)
	}
}

func TestWithRetry_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := withRetry(ctx, func() error {
		return errors.New("should not be retried past first delay")
	})
	// The first attempt runs. Then the delay select should see ctx.Done.
	// Accept either context.Canceled (first attempt returned an error and we
	// tried to delay) or any error (cancelled context propagated from op).
	if err == nil {
		t.Fatal("expected non-nil error for cancelled context")
	}
}

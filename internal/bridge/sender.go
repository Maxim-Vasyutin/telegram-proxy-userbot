package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/tgerr"
)

// ErrChatForbidden is returned by withRetry when the bot has been removed
// from the target chat (CHAT_WRITE_FORBIDDEN). The caller should mark the
// pair as disabled so further events are silently skipped.
var ErrChatForbidden = errors.New("bridge: bot removed from target chat (CHAT_WRITE_FORBIDDEN)")

// withRetry executes op, retrying up to 3 times on transient errors.
// Delays between attempts: 1 s, 2 s, 4 s.
//
// Special handling:
//   - FLOOD_WAIT: sleep the required duration, retry exactly once more, then
//     return whatever the retry returns.
//   - CHAT_WRITE_FORBIDDEN: return ErrChatForbidden immediately (no retry).
//   - All other errors: retry with back-off.
func withRetry(ctx context.Context, op func() error) error {
	retryDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryDelays[attempt-1]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err := op()
		if err == nil {
			return nil
		}

		if tgerr.Is(err, "CHAT_WRITE_FORBIDDEN") {
			return ErrChatForbidden
		}

		if wait, ok := tgerr.AsFloodWait(err); ok {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
			// Single retry after FLOOD_WAIT; don't loop further.
			if retryErr := op(); retryErr == nil {
				return nil
			} else {
				return retryErr
			}
		}

		lastErr = err
	}
	return lastErr
}

package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)


// messageKey identifies a single message within a chat, used as cache key.
type messageKey struct {
	ChatID    int64
	MessageID int64
}

// reactionState is the cached reaction set for one message.
type reactionState struct {
	// Reactions is a sorted deduplicated slice of standard emoji strings.
	Reactions []string
	UpdatedAt time.Time
}

// reactionsCache holds the last-known aggregate reaction set for source
// messages. A background goroutine evicts entries older than 24 hours.
type reactionsCache struct {
	mu    sync.RWMutex
	items map[messageKey]reactionState
}

func (c *reactionsCache) get(key messageKey) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.items[key]
	if !ok {
		return nil
	}
	cp := make([]string, len(s.Reactions))
	copy(cp, s.Reactions)
	return cp
}

func (c *reactionsCache) set(key messageKey, reactions []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = reactionState{Reactions: reactions, UpdatedAt: time.Now()}
}

// cleanup removes entries older than 24 hours and returns the count removed.
func (c *reactionsCache) cleanup() int {
	threshold := time.Now().Add(-24 * time.Hour)
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, s := range c.items {
		if s.UpdatedAt.Before(threshold) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}

// runCacheCleanup runs the hourly cleanup loop until the process exits.
// Phase 10 will hook this into the proper application lifecycle context.
func (b *Impl) runCacheCleanup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if n := b.rCache.cleanup(); n > 0 {
			slog.Info("reactions cache cleaned", "removed", n)
		}
	}
}

// OnReactions mirrors the aggregate reaction set of a source message to its
// mirrored counterpart. Standard emoji reactions are relayed; premium custom
// emoji reactions are silently skipped (see SPEC §5.3).
//
// The cache stores the full reaction set seen on each source message. On every
// update the new set is compared with the cached one; if they differ the bot
// calls messages.sendReaction on the mirror with the complete new set
// (which replaces all of the bot's existing reactions in a single call).
func (b *Impl) OnReactions(ctx context.Context, ev ReactionsEvent) error {
	// Build the current standard-emoji set, filtering premium custom reactions.
	currentSet := make([]string, 0, len(ev.Reactions))
	for _, r := range ev.Reactions {
		if r.Custom {
			slog.Debug("custom reaction skipped",
				"chat_id", ev.ChatID, "message_id", ev.MessageID)
			continue
		}
		if r.Emoji != "" {
			currentSet = append(currentSet, r.Emoji)
		}
	}
	sort.Strings(currentSet)
	currentSet = dedupStrings(currentSet) // guard against duplicates

	key := messageKey{ChatID: ev.ChatID, MessageID: ev.MessageID}
	prevSet := b.rCache.get(key)

	// Update cache early so even a no-op refreshes UpdatedAt (TTL stays live).
	b.rCache.set(key, currentSet)

	added := setDiff(currentSet, prevSet)
	removed := setDiff(prevSet, currentSet)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}

	// Locate the mirror of the source message.
	mirrorChatID, mirrorMsgID, found, err := b.findReactionMirror(ctx, ev.ChatID, ev.MessageID)
	if err != nil {
		return err
	}
	if !found {
		slog.Warn("reaction: mirror not found",
			"chat_id", ev.ChatID, "message_id", ev.MessageID)
		return nil
	}

	mirrorPeer, ok := b.peerByID[mirrorChatID]
	if !ok {
		slog.Error("reaction: no peer resolved for mirror chat",
			"mirror_chat_id", mirrorChatID)
		return nil
	}

	if b.api == nil {
		return fmt.Errorf("bridge: api client not initialized")
	}

	// Build the reaction list for SendReaction.
	// SendReaction replaces the bot's entire reaction set on the message, so
	// we pass currentSet (the new aggregate, minus custom). Empty slice = remove all.
	reactions := make([]tg.ReactionClass, len(currentSet))
	for i, emoji := range currentSet {
		reactions[i] = &tg.ReactionEmoji{Emoticon: emoji}
	}

	if err := b.sendReaction(ctx, mirrorPeer, int(mirrorMsgID), reactions); err != nil {
		slog.Warn("reaction: send failed", "error", err)
		return nil // reactions are best-effort; don't propagate
	}

	slog.Debug("reaction relayed",
		"chat_id", ev.ChatID,
		"message_id", ev.MessageID,
		"added", added,
		"removed", removed,
	)
	return nil
}

// sendReaction calls messages.sendReaction, using withRetry for FLOOD_WAIT
// and other transient errors.
func (b *Impl) sendReaction(
	ctx context.Context,
	peer tg.InputPeerClass,
	msgID int,
	reactions []tg.ReactionClass,
) error {
	req := &tg.MessagesSendReactionRequest{
		Peer:     peer,
		MsgID:    msgID,
		Reaction: reactions,
	}

	return withRetry(ctx, func() error {
		_, err := b.api.MessagesSendReaction(ctx, req)
		if tgerr.Is(err, "REACTION_INVALID") {
			slog.Warn("reaction: emoji not allowed in chat", "msg_id", msgID)
			return nil
		}
		return err
	})
}

// findReactionMirror looks up the mirror message for a given source message.
// It checks both directions: source→target and target→source.
func (b *Impl) findReactionMirror(
	ctx context.Context,
	chatID, messageID int64,
) (mirrorChatID, mirrorMsgID int64, found bool, err error) {
	m, err := b.st.FindBySource(ctx, chatID, messageID)
	if err != nil {
		return 0, 0, false, err
	}
	if m != nil {
		return m.TargetChatID, m.TargetMessageID, true, nil
	}

	m, err = b.st.FindByTarget(ctx, chatID, messageID)
	if err != nil {
		return 0, 0, false, err
	}
	if m != nil {
		return m.SourceChatID, m.SourceMessageID, true, nil
	}

	return 0, 0, false, nil
}

// setDiff returns elements in a that are not in b (set subtraction a − b).
// Both slices must be sorted.
func setDiff(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, s := range b {
		bSet[s] = struct{}{}
	}
	var diff []string
	for _, s := range a {
		if _, ok := bSet[s]; !ok {
			diff = append(diff, s)
		}
	}
	return diff
}

// dedupStrings removes consecutive duplicates from a sorted slice.
func dedupStrings(sorted []string) []string {
	if len(sorted) == 0 {
		return sorted
	}
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

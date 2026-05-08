package bridge

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// ---------- setDiff tests ----------

func TestSetDiff_Added(t *testing.T) {
	prev := []string{"👍"}
	curr := []string{"👍", "❤️"}
	got := setDiff(curr, prev)
	want := []string{"❤️"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setDiff added: got %v, want %v", got, want)
	}
}

func TestSetDiff_Removed(t *testing.T) {
	prev := []string{"👍", "❤️"}
	curr := []string{"❤️"}
	got := setDiff(prev, curr) // prev − curr = removed
	want := []string{"👍"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setDiff removed: got %v, want %v", got, want)
	}
}

func TestSetDiff_Changed(t *testing.T) {
	// 👍 replaced by 🔥
	prev := []string{"👍"}
	curr := []string{"🔥"}

	added := setDiff(curr, prev)
	removed := setDiff(prev, curr)

	wantAdded := []string{"🔥"}
	wantRemoved := []string{"👍"}

	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("changed added: got %v, want %v", added, wantAdded)
	}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("changed removed: got %v, want %v", removed, wantRemoved)
	}
}

func TestSetDiff_NoChange(t *testing.T) {
	set := []string{"👍", "❤️"}
	if d := setDiff(set, set); len(d) != 0 {
		t.Fatalf("expected empty diff, got %v", d)
	}
}

// ---------- reactionsCache TTL / cleanup tests ----------

func TestReactionsCache_Cleanup_RemovesOld(t *testing.T) {
	c := &reactionsCache{items: make(map[messageKey]reactionState)}

	old := messageKey{ChatID: 1, MessageID: 10}
	fresh := messageKey{ChatID: 1, MessageID: 20}

	// Insert an entry 25 hours old (beyond the 24 h TTL).
	c.mu.Lock()
	c.items[old] = reactionState{
		Reactions: []string{"👍"},
		UpdatedAt: time.Now().Add(-25 * time.Hour),
	}
	// Insert a fresh entry updated 1 minute ago.
	c.items[fresh] = reactionState{
		Reactions: []string{"❤️"},
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	c.mu.Unlock()

	removed := c.cleanup()

	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	c.mu.RLock()
	_, oldExists := c.items[old]
	_, freshExists := c.items[fresh]
	c.mu.RUnlock()

	if oldExists {
		t.Error("old entry should have been removed")
	}
	if !freshExists {
		t.Error("fresh entry should have been kept")
	}
}

func TestReactionsCache_Cleanup_KeepsAll(t *testing.T) {
	c := &reactionsCache{items: make(map[messageKey]reactionState)}

	for i := 0; i < 5; i++ {
		k := messageKey{ChatID: 1, MessageID: int64(i)}
		c.mu.Lock()
		c.items[k] = reactionState{
			Reactions: []string{"👍"},
			UpdatedAt: time.Now(),
		}
		c.mu.Unlock()
	}

	if n := c.cleanup(); n != 0 {
		t.Fatalf("expected 0 removed, got %d", n)
	}
}

// ---------- dedupStrings helper test ----------

func TestDedupStrings(t *testing.T) {
	in := []string{"a", "a", "b", "b", "b", "c"}
	got := dedupStrings(in)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupStrings: got %v, want %v", got, want)
	}
}

// ---------- OnReactions filtering test ----------

// TestOnReactions_CustomFiltered verifies that custom (premium) reactions are
// silently dropped and do not reach the API or storage layer.
func TestOnReactions_CustomFiltered(t *testing.T) {
	st := &mockStorage{
		bySource: map[[2]int64]*storage.MessageMapping{},
		byTarget: map[[2]int64]*storage.MessageMapping{},
	}
	b := newEditTestImpl(st) // reuses helper from edits_test.go

	ev := ReactionsEvent{
		ChatID:    100,
		MessageID: 1,
		Reactions: []ReactionItem{
			{Emoji: "", Custom: true},
			{Emoji: "", Custom: true},
		},
	}

	// All reactions are custom → currentSet is empty → delta is zero → return nil.
	if err := b.OnReactions(context.Background(), ev); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// The cache entry should exist but hold an empty slice.
	key := messageKey{ChatID: 100, MessageID: 1}
	cached := b.rCache.get(key)
	if len(cached) != 0 {
		t.Fatalf("expected empty cached set, got %v", cached)
	}
}


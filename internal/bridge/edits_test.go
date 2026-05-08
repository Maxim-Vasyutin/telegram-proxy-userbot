package bridge

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// newEditTestImpl builds a minimal *Impl for edit/delete unit tests.
// api is left nil: tests that exercise paths not reaching the API call verify
// the storage-layer and filtering logic only. Tests that reach the API call
// confirm the correct path was taken by observing a non-nil error return.
func newEditTestImpl(st storage.Storage) *Impl {
	pairs := []config.PairConfig{
		{Key: "pair_a", MerchantChatID: 100, SupportChatID: 200},
	}
	b := New(st, nil, nil, pairs, 999 /*selfID*/, "+1234567890")
	b.peerByID[100] = &tg.InputPeerChannel{ChannelID: 1, AccessHash: 11}
	b.peerByID[200] = &tg.InputPeerChannel{ChannelID: 2, AccessHash: 22}
	return b
}

// TestOnMessageEdited_MappingNotFound verifies that an edit for a message with
// no row in message_mappings is silently ignored (returns nil, no panic).
func TestOnMessageEdited_MappingNotFound(t *testing.T) {
	st := &mockStorage{
		bySource: map[[2]int64]*storage.MessageMapping{},
	}
	b := newEditTestImpl(st)

	ev := EditEvent{ChatID: 100, MessageID: 42, FromID: 123, Text: "new text"}
	if err := b.OnMessageEdited(context.Background(), ev); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestOnMessageEdited_Found verifies that when a mapping exists the function
// proceeds past storage to the API call. A nil api makes that call return a
// non-nil error, confirming the found-path was taken rather than an early exit.
func TestOnMessageEdited_Found(t *testing.T) {
	st := &mockStorage{
		bySource: map[[2]int64]*storage.MessageMapping{
			{100, 42}: {
				PairKey:         "pair_a",
				SourceChatID:    100,
				SourceMessageID: 42,
				TargetChatID:    200,
				TargetMessageID: 99,
				Direction:       storage.DirMerchantToSupport,
			},
		},
	}
	b := newEditTestImpl(st)

	ev := EditEvent{ChatID: 100, MessageID: 42, FromID: 123, Text: "updated"}
	err := b.OnMessageEdited(context.Background(), ev)
	if err == nil {
		t.Fatal("expected non-nil error from nil api call, got nil")
	}
}

// TestOnMessageDeleted_MappingNotFound verifies that a delete for an unknown
// message is silently ignored (returns nil, no panic).
func TestOnMessageDeleted_MappingNotFound(t *testing.T) {
	st := &mockStorage{
		bySource: map[[2]int64]*storage.MessageMapping{},
	}
	b := newEditTestImpl(st)

	ev := DeleteEvent{ChatID: 100, MessageIDs: []int64{55, 56}}
	if err := b.OnMessageDeleted(context.Background(), ev); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestOnMessageDeleted_Found verifies that when a mapping exists the function
// calls FindBySource with the correct coordinates. The delete API call is
// silently absorbed on nil api; the method returns nil (best-effort loop).
func TestOnMessageDeleted_Found(t *testing.T) {
	calledWithChatID := int64(-1)
	calledWithMsgID := int64(-1)

	st := &mockStorage{
		bySource: map[[2]int64]*storage.MessageMapping{
			{100, 77}: {
				PairKey:         "pair_a",
				SourceChatID:    100,
				SourceMessageID: 77,
				TargetChatID:    200,
				TargetMessageID: 88,
				Direction:       storage.DirMerchantToSupport,
			},
		},
		findBySourceFn: func(chatID, messageID int64) {
			calledWithChatID = chatID
			calledWithMsgID = messageID
		},
	}
	b := newEditTestImpl(st)

	ev := DeleteEvent{ChatID: 100, MessageIDs: []int64{77}}
	if err := b.OnMessageDeleted(context.Background(), ev); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if calledWithChatID != 100 || calledWithMsgID != 77 {
		t.Fatalf("FindBySource called with (%d,%d), want (100,77)",
			calledWithChatID, calledWithMsgID)
	}
}

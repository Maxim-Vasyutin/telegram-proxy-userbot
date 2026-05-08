package bridge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// OnMessageEdited relays an edit event to the mirrored message in the target
// chat. Called by the tg dispatcher for UpdateEditMessage /
// UpdateEditChannelMessage events.
//
// Loop-prevention: when the bridge edits a message in target_chat, Telegram
// generates an UpdateEditMessage for the bot. That edit's FromID equals
// selfID, so IsRelevant returns false and the loop is broken at the first
// check below.
func (b *Impl) OnMessageEdited(ctx context.Context, ev EditEvent) error {
	if !IsRelevant(ev.ChatID, ev.FromID, b.selfID, b.pairsByChat) {
		return nil
	}

	pair := b.pairsByChat[ev.ChatID]

	b.mu.RLock()
	_, disabled := b.disabledPairs[pair.Key]
	b.mu.RUnlock()
	if disabled {
		return nil
	}

	m, err := b.st.FindBySource(ctx, ev.ChatID, ev.MessageID)
	if err != nil {
		return err
	}
	if m == nil {
		slog.Debug("edit: source not in mappings",
			"chat_id", ev.ChatID, "message_id", ev.MessageID)
		return nil
	}

	targetPeer, ok := b.peerByID[m.TargetChatID]
	if !ok {
		slog.Error("edit: no peer resolved for target chat",
			"pair_key", m.PairKey, "target_chat_id", m.TargetChatID)
		return nil
	}

	req := &tg.MessagesEditMessageRequest{
		Peer:    targetPeer,
		ID:      int(m.TargetMessageID),
		Message: ev.Text,
	}
	if len(ev.Entities) > 0 {
		req.Entities = convertEntitiesToTG(ev.Entities)
	}
	if ev.NewMedia != nil {
		req.Media = buildInputMedia(ev.NewMedia)
	}

	if b.api == nil {
		return fmt.Errorf("bridge: api client not initialized")
	}
	if _, err = b.api.MessagesEditMessage(ctx, req); err != nil {
		if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
			slog.Debug("edit: message not modified",
				"pair_key", m.PairKey, "target_msg_id", m.TargetMessageID)
			return nil
		}
		if tgerr.Is(err, "MESSAGE_ID_INVALID") {
			slog.Debug("edit: target already gone",
				"pair_key", m.PairKey, "target_msg_id", m.TargetMessageID)
			return nil
		}
		slog.Error("failed to relay edit",
			"pair_key", m.PairKey,
			"source_msg_id", ev.MessageID,
			"target_msg_id", m.TargetMessageID,
			"error", err,
		)
		return err
	}

	slog.Info("message edit relayed",
		"pair_key", m.PairKey,
		"source_msg_id", ev.MessageID,
		"target_msg_id", m.TargetMessageID,
	)
	return nil
}

// OnMessageDeleted relays delete events to the mirrored messages in the target
// chat. Called by the tg dispatcher for UpdateDeleteMessages /
// UpdateDeleteChannelMessages events.
//
// Loop-prevention: when the bridge deletes a message in target_chat, Telegram
// generates an UpdateDeleteMessages for the bot. FindBySource looks up by
// (source_chat_id, source_message_id) — the deleted target message ID is
// stored as target_message_id, not source_message_id, so FindBySource returns
// nil and the loop is automatically broken.
func (b *Impl) OnMessageDeleted(ctx context.Context, ev DeleteEvent) error {
	// UpdateDeleteMessages (private/basic-group deletes) carries no chat ID.
	// Since all configured pairs are supergroups, this event never matches.
	if ev.ChatID == 0 {
		return nil
	}

	if _, ok := b.pairsByChat[ev.ChatID]; !ok {
		return nil
	}

	pair := b.pairsByChat[ev.ChatID]

	b.mu.RLock()
	_, disabled := b.disabledPairs[pair.Key]
	b.mu.RUnlock()
	if disabled {
		return nil
	}

	deleted := 0
	for _, msgID := range ev.MessageIDs {
		m, err := b.st.FindBySource(ctx, ev.ChatID, msgID)
		if err != nil {
			slog.Error("delete: storage lookup failed",
				"pair_key", pair.Key, "chat_id", ev.ChatID, "message_id", msgID, "error", err)
			continue
		}
		if m == nil {
			slog.Debug("delete: source not in mappings",
				"chat_id", ev.ChatID, "message_id", msgID)
			continue
		}

		targetPeer, ok := b.peerByID[m.TargetChatID]
		if !ok {
			slog.Error("delete: no peer resolved for target chat",
				"pair_key", m.PairKey, "target_chat_id", m.TargetChatID)
			continue
		}

		if b.api == nil {
			slog.Error("delete: api client not initialized", "pair_key", m.PairKey)
			continue
		}

		var delErr error
		switch p := targetPeer.(type) {
		case *tg.InputPeerChannel:
			_, delErr = b.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
				Channel: &tg.InputChannel{
					ChannelID:  p.ChannelID,
					AccessHash: p.AccessHash,
				},
				ID: []int{int(m.TargetMessageID)},
			})
		default:
			_, delErr = b.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
				ID: []int{int(m.TargetMessageID)},
			})
		}

		if delErr != nil {
			if tgerr.Is(delErr, "MESSAGE_ID_INVALID") {
				slog.Debug("delete: target already gone",
					"pair_key", m.PairKey, "target_msg_id", m.TargetMessageID)
				continue
			}
			slog.Error("failed to relay delete",
				"pair_key", m.PairKey,
				"source_msg_id", msgID,
				"target_msg_id", m.TargetMessageID,
				"error", delErr,
			)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		slog.Info("message delete relayed", "pair_key", pair.Key, "count", deleted)
	}
	return nil
}

// buildInputMedia converts a MediaRef (from an edit event) into the
// tg.InputMediaClass required by MessagesEditMessageRequest.Media.
// Photos are sent as InputMediaPhoto; everything else as InputMediaDocument.
func buildInputMedia(ref *MediaRef) tg.InputMediaClass {
	if ref.Type == MediaTypePhoto {
		return &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            ref.FileID,
				AccessHash:    ref.AccessHash,
				FileReference: ref.FileRef,
			},
		}
	}
	return &tg.InputMediaDocument{
		ID: &tg.InputDocument{
			ID:            ref.FileID,
			AccessHash:    ref.AccessHash,
			FileReference: ref.FileRef,
		},
	}
}

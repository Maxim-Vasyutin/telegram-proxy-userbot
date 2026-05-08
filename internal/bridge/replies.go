package bridge

import (
	"context"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// resolveReplyTarget looks up the message_mappings table to find which
// message ID to use as reply_to when relaying a reply.
//
// sourceChatID  — the chat where the incoming message (with the reply) was sent.
// replyToMsgID  — the message being replied to, as seen in sourceChatID.
// direction     — the relay direction (determines which field to return).
//
// Returns:
//
//	targetReplyID — the message ID to use as reply_to in the TARGET chat.
//	found         — false if no mapping was found (caller should send without reply).
//	err           — non-nil only on storage errors.
func resolveReplyTarget(
	ctx context.Context,
	st storage.Storage,
	sourceChatID int64,
	replyToMsgID int64,
	direction storage.Direction,
) (targetReplyID int64, found bool, err error) {
	// CASE 1: The sender is replying to a RELAYED message (i.e. a message the
	// userbot forwarded into this chat). That relayed message appears in
	// message_mappings as target_chat_id=sourceChatID, target_message_id=replyToMsgID.
	// We want to reply to the SOURCE of that relay — source_message_id.
	m, err := st.FindByTarget(ctx, sourceChatID, replyToMsgID)
	if err != nil {
		return 0, false, err
	}
	if m != nil {
		return m.SourceMessageID, true, nil
	}

	// CASE 2: The sender is replying to their own original message (i.e. a message
	// that was relayed FROM this chat TO the other side). It appears in
	// message_mappings as source_chat_id=sourceChatID, source_message_id=replyToMsgID.
	// We want to reply to the TARGET of that relay — target_message_id.
	m, err = st.FindBySource(ctx, sourceChatID, replyToMsgID)
	if err != nil {
		return 0, false, err
	}
	if m != nil {
		return m.TargetMessageID, true, nil
	}

	return 0, false, nil
}

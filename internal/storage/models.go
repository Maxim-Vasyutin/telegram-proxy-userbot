package storage

import "time"

// MessageMapping is the in-memory representation of a single row in the
// message_mappings table. It pairs the coordinates of an original message
// with the coordinates of the relayed (mirror) message.
type MessageMapping struct {
	// ID is the auto-generated primary key (BIGSERIAL).
	ID int64 `db:"id"`

	// PairKey is the human-readable key from config.yaml (e.g. "merch_1").
	PairKey string `db:"pair_key"`

	// SourceChatID is the Telegram chat ID where the original message was sent.
	SourceChatID int64 `db:"source_chat_id"`

	// SourceMessageID is the Telegram message ID in the source chat.
	SourceMessageID int64 `db:"source_message_id"`

	// TargetChatID is the Telegram chat ID where the relayed message was sent.
	TargetChatID int64 `db:"target_chat_id"`

	// TargetMessageID is the Telegram message ID in the target chat.
	TargetMessageID int64 `db:"target_message_id"`

	// Direction indicates which side originated the message.
	Direction Direction `db:"direction"`

	// MediaGroupID is nil for standalone messages; for album messages it holds
	// the shared grouped_id that binds all album elements together.
	MediaGroupID *string `db:"media_group_id"`

	// AccountPhone is the phone number of the userbot account that processed
	// this message. Stored for auditing purposes; the mapping is shared between
	// accounts.
	AccountPhone string `db:"account_phone"`

	// CreatedAt is the time the record was inserted (set by the DB default NOW()).
	CreatedAt time.Time `db:"created_at"`
}

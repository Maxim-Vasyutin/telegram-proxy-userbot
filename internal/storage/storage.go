// Package storage defines the Storage interface and related types for
// persisting Telegram message mappings between paired groups.
package storage

import (
	"context"
	"errors"
)

// Direction indicates which side originated the relayed message.
type Direction string

const (
	// DirMerchantToSupport is used when the event came from the merchant chat.
	DirMerchantToSupport Direction = "merchant_to_support"
	// DirSupportToMerchant is used when the event came from the support chat.
	DirSupportToMerchant Direction = "support_to_merchant"
)

// ErrDuplicate is returned by SaveMapping when a record with the same
// (source_chat_id, source_message_id) already exists in the database.
var ErrDuplicate = errors.New("storage: mapping already exists")

// Storage is the persistence interface for message mappings.
// All methods accept a context as the first argument to support cancellation
// and deadline propagation.
type Storage interface {
	// SaveMapping inserts a new MessageMapping record.
	// Returns ErrDuplicate if a mapping for the same source message already exists
	// (unique constraint on (source_chat_id, source_message_id)).
	SaveMapping(ctx context.Context, m MessageMapping) error

	// FindBySource looks up a mapping by the coordinates of the originating message.
	// Used for edit/delete/reaction events.
	// Returns (nil, nil) when no matching record is found.
	FindBySource(ctx context.Context, chatID, messageID int64) (*MessageMapping, error)

	// FindByTarget looks up a mapping by the coordinates of the relayed (mirror) message.
	// Used to resolve reply targets when a user replies to a relayed message.
	// Returns (nil, nil) when no matching record is found.
	FindByTarget(ctx context.Context, chatID, messageID int64) (*MessageMapping, error)

	// FindByMediaGroup returns all mappings that share the given media_group_id.
	// Used for album-level edit and delete operations.
	// Returns an empty slice (not nil) when no records are found.
	FindByMediaGroup(ctx context.Context, mediaGroupID string) ([]MessageMapping, error)

	// Ping checks the database is reachable. Used by the health-check goroutine.
	Ping(ctx context.Context) error

	// Close releases all resources held by the storage implementation,
	// including the underlying connection pool.
	Close() error
}

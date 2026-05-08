// Package bridge wires together the Telegram update dispatcher and the
// storage layer to relay messages between paired merchant and support chats.
//
// Phase 4 adds bidirectional relay and media forwarding (photo, video,
// document, voice, video_note, sticker, GIF).
package bridge

import (
	"context"
	"sync"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// Bridge is the interface that the tg.Client dispatcher calls for every
// relevant incoming update. It contains exactly the five handlers defined
// in SPEC §3.2 — no lifecycle methods.
type Bridge interface {
	OnNewMessage(ctx context.Context, ev MessageEvent) error
	OnAlbum(ctx context.Context, ev AlbumEvent) error
	OnMessageEdited(ctx context.Context, ev EditEvent) error
	OnMessageDeleted(ctx context.Context, ev DeleteEvent) error
	OnReactions(ctx context.Context, ev ReactionsEvent) error
}

// New builds an Impl ready for use.
//
// pairs is the slice from config.yaml; selfID is the user ID of the userbot
// account (used to ignore messages sent by the account itself);
// accountPhone is stored verbatim in every MessageMapping for auditing.
// api is the raw tg.Client used for downloading and low-level API calls.
//
// Call ResolvePeers on the returned *Impl before relaying starts.
func New(
	st storage.Storage,
	sender *message.Sender,
	api *tg.Client,
	pairs []config.PairConfig,
	selfID int64,
	accountPhone string,
) *Impl {
	// Build a map keyed by both sides of each pair so every lookup is O(1).
	pairsByChat := make(map[int64]Pair, len(pairs)*2)
	for _, p := range pairs {
		pairsByChat[p.MerchantChatID] = Pair{
			Key:            p.Key,
			MerchantChatID: p.MerchantChatID,
			SupportChatID:  p.SupportChatID,
			TargetChatID:   p.SupportChatID,
		}
		pairsByChat[p.SupportChatID] = Pair{
			Key:            p.Key,
			MerchantChatID: p.MerchantChatID,
			SupportChatID:  p.SupportChatID,
			TargetChatID:   p.MerchantChatID,
		}
	}

	b := &Impl{
		st:            st,
		sender:        sender,
		api:           api,
		selfID:        selfID,
		accountPhone:  accountPhone,
		pairsByChat:   pairsByChat,
		peerByID:      make(map[int64]tg.InputPeerClass),
		disabledPairs: make(map[string]struct{}),
		rCache:        &reactionsCache{items: make(map[messageKey]reactionState)},
	}

	// Start a background goroutine to evict stale reaction cache entries.
	// Phase 10 will integrate this with the proper application lifecycle.
	go b.runCacheCleanup()

	return b
}

// Impl is the concrete implementation of Bridge.
// It is exported so that callers (e.g. cmd/userbot/main.go) can call
// ResolvePeers before wiring the bridge into the tg.Client dispatcher.
type Impl struct {
	st           storage.Storage
	sender       *message.Sender
	// api is the raw MTProto client; used for low-level calls (e.g. send with
	// entities) and by downloader/uploader.  Concrete type is fine here —
	// the SPEC §3.2 constraint applies to the Bridge interface and events.go,
	// not to private fields of the concrete Impl.
	api          *tg.Client
	selfID       int64
	accountPhone string

	// pairsByChat is keyed by both merchant and support chat IDs.
	pairsByChat map[int64]Pair

	// peerByID maps full chat IDs (e.g. -1001234567890) to pre-resolved
	// InputPeerClass values. Populated by ResolvePeers before relaying starts.
	peerByID map[int64]tg.InputPeerClass

	// mu protects disabledPairs.
	mu sync.RWMutex
	// disabledPairs holds pair keys where the bot has been removed from a chat
	// (CHAT_WRITE_FORBIDDEN). Relay is silently skipped for disabled pairs.
	disabledPairs map[string]struct{}

	// rCache stores the last-known aggregate reaction set for each source
	// message. Used by OnReactions to compute the delta and mirror reactions.
	rCache *reactionsCache
}

// ResolvePeers fetches access hashes for every chat that appears in the
// configured pairs. It must be called once after Connect and before any
// messages arrive.
//
// For each unique chat ID the method calls channels.getChannels with
// AccessHash=0; Telegram returns the real access hash as long as the
// userbot is a member of the channel/supergroup.
func (b *Impl) ResolvePeers(ctx context.Context, api *tg.Client) error {
	seen := make(map[int64]struct{})
	for chatID := range b.pairsByChat {
		seen[chatID] = struct{}{}
	}

	for chatID := range seen {
		// Telegram supergroup / channel IDs are stored in config as
		// negative values in the -100XXXXXXXXXX form.
		// Strip the -100 prefix:
		//   channelID = (-chatID) - 1_000_000_000_000
		// e.g. -1001234567890 → 1001234567890 - 1000000000000 = 1234567890
		channelID := (-chatID) - 1_000_000_000_000

		req := &tg.InputChannel{
			ChannelID:  channelID,
			AccessHash: 0,
		}

		result, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{req})
		if err != nil {
			return err
		}

		chats := result.GetChats()
		if len(chats) == 0 {
			continue
		}

		ch, ok := chats[0].(*tg.Channel)
		if !ok {
			continue
		}

		b.peerByID[chatID] = &tg.InputPeerChannel{
			ChannelID:  ch.ID,
			AccessHash: ch.AccessHash,
		}
	}

	return nil
}


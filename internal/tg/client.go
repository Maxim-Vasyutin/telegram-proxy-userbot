// Package tg wraps the github.com/gotd/td MTProto client and exposes a
// minimal lifecycle API used by the rest of the userbot:
//
//   - New constructs a Client bound to a single Telegram account.
//   - Authorize runs the interactive auth flow used by --auth.
//   - Connect brings the client online in the background and starts the
//     updates.Manager goroutine for catch_up.
//   - Disconnect tears everything down with a bounded timeout.
//
// Phase 4 adds bidirectional relay and media forwarding.
package tg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/contrib/bg"
	boltstor "github.com/gotd/contrib/bbolt"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"go.etcd.io/bbolt"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/bridge"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
)

// albumBuffer accumulates *tg.Message items that share the same GroupedID.
// A 500 ms flush timer fires after the last item arrives; if 10 items
// accumulate before the timer fires the flush is triggered immediately
// (Telegram's hard limit per album is 10).
type albumBuffer struct {
	mu      sync.Mutex
	items   []*tg.Message
	timer   *time.Timer
	chatID  int64
	fromID  int64
	groupID int64
}

// CodePrompt asks the operator to enter the SMS/Telegram-app code.
type CodePrompt func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error)

// PasswordPrompt asks the operator to enter the cloud (2FA) password.
type PasswordPrompt func(ctx context.Context) (string, error)

// Client is a per-account wrapper around telegram.Client.
//
// One Client corresponds to exactly one Telegram account (one phone, one
// .session file, one BoltDB state file). Lifecycle is:
//
//	c, _ := tg.New(...)
//	c.Connect(ctx)        // brings the account online
//	defer c.Disconnect(...)
//
// or, for the one-shot --auth path:
//
//	c, _ := tg.New(...)
//	c.Authorize(ctx, codePrompt, passwordPrompt)
type Client struct {
	phone       string
	sessionPath string
	boltPath    string
	apiID       int
	apiHash     string

	// br is the bridge for relaying messages. May be nil on the --auth path.
	br bridge.Bridge

	// Constructed in New, owned by Client until Disconnect.
	tdClient   *telegram.Client
	dispatcher tg.UpdateDispatcher
	gaps       *updates.Manager
	boltDB     *bbolt.DB

	// selfID is the user ID of the authenticated account, set in Connect.
	selfID int64

	// albums buffers incoming messages that share a GroupedID until the
	// album is complete (500 ms quiet period or 10 items).
	// Key: grouped_id (int64) → *albumBuffer
	albums sync.Map

	// Set by Connect, cleared by Disconnect.
	mu      sync.Mutex
	stopBG  bg.StopFunc
	cancel  context.CancelFunc
	gapsErr chan error
}

// New constructs a Client for the given account configuration.
//
// boltPath is the absolute path to the BoltDB file used by the
// updates.Manager to persist pts/qts/seq across restarts. It is shared
// between accounts in the spec, but each Client opens its own handle —
// we make boltPath per-account by appending the phone if the caller
// passes a directory-style path. To keep behaviour simple here we just
// use boltPath verbatim; the orchestrator (Phase 9) decides the path.
//
// br is the Bridge used to relay incoming messages. Pass nil on the --auth
// path (no relay is needed during interactive authorisation).
func New(cfg config.AccountConfig, apiID int, apiHash string, boltPath string, br bridge.Bridge) (*Client, error) {
	if cfg.Phone == "" {
		return nil, errors.New("tg: account phone must not be empty")
	}
	if cfg.SessionPath == "" {
		return nil, errors.New("tg: account session_path must not be empty")
	}
	if apiID == 0 || apiHash == "" {
		return nil, errors.New("tg: api_id and api_hash must be provided")
	}
	if boltPath == "" {
		return nil, errors.New("tg: bolt state path must not be empty")
	}

	// Ensure parent directories exist for both files. The caller is
	// expected to have arranged write permissions; we just create the
	// directories so the first run does not fail.
	if err := os.MkdirAll(filepath.Dir(cfg.SessionPath), 0o700); err != nil {
		return nil, fmt.Errorf("tg: create session dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(boltPath), 0o700); err != nil {
		return nil, fmt.Errorf("tg: create state dir: %w", err)
	}

	// Open BoltDB for the updates state. The handle stays alive for the
	// lifetime of the Client and is closed in Disconnect.
	db, err := bbolt.Open(boltPath, 0o600, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("tg: open bolt %q: %w", boltPath, err)
	}

	// Updates state storage backed by bbolt — survives restarts so that
	// updates.Manager can resume from the last known pts/qts/seq instead
	// of replaying everything (or, worse, missing updates entirely).
	stateStorage := boltstor.NewStateStorage(db)

	// client is a forward-reference populated right after construction.
	// The dispatcher closure captures it by pointer so that SetBridge can
	// inject the real bridge after Connect returns with the selfID.
	client := &Client{
		phone:       cfg.Phone,
		sessionPath: cfg.SessionPath,
		boltPath:    boltPath,
		apiID:       apiID,
		apiHash:     apiHash,
		br:          br,
		boltDB:      db,
	}

	// Dispatcher receives typed updates from updates.Manager.
	// It reads c.br under the mutex so that SetBridge is race-free.
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		client.mu.Lock()
		curBr := client.br
		client.mu.Unlock()
		if curBr == nil {
			return nil
		}
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}

		// Album messages carry a non-zero GroupedID — buffer them and flush
		// once the group is complete rather than relaying each item alone.
		if msg.GroupedID != 0 {
			client.handleAlbumMessage(ctx, msg, curBr)
			return nil
		}

		ev := bridge.MessageEvent{
			ChatID:    peerID(msg.PeerID),
			MessageID: int64(msg.ID),
			FromID:    fromPeerID(msg.FromID),
			Text:      msg.Message,
			Entities:  convertEntities(msg.Entities),
			Media:     convertMedia(msg.Media),
		}
		if msg.ReplyTo != nil {
			if replyMsg, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				if replyMsgID, hasID := replyMsg.GetReplyToMsgID(); hasID {
					id := int64(replyMsgID)
					ev.ReplyToMsgID = &id
				}
			}
		}
		return curBr.OnNewMessage(ctx, ev)
	})

	// OnEditMessage / OnEditChannelMessage — relay text/media edits.
	editHandler := func(ctx context.Context, msg *tg.Message) error {
		client.mu.Lock()
		curBr := client.br
		client.mu.Unlock()
		if curBr == nil || msg == nil {
			return nil
		}
		ev := bridge.EditEvent{
			ChatID:    peerID(msg.PeerID),
			MessageID: int64(msg.ID),
			FromID:    fromPeerID(msg.FromID),
			Text:      msg.Message,
			Entities:  convertEntities(msg.Entities),
			NewMedia:  convertMedia(msg.Media),
		}
		return curBr.OnMessageEdited(ctx, ev)
	}

	dispatcher.OnEditMessage(func(ctx context.Context, _ tg.Entities, u *tg.UpdateEditMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		return editHandler(ctx, msg)
	})

	dispatcher.OnEditChannelMessage(func(ctx context.Context, _ tg.Entities, u *tg.UpdateEditChannelMessage) error {
		msg, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		return editHandler(ctx, msg)
	})

	// OnDeleteMessages — private/basic-group deletes (ChatID not available).
	dispatcher.OnDeleteMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteMessages) error {
		client.mu.Lock()
		curBr := client.br
		client.mu.Unlock()
		if curBr == nil {
			return nil
		}
		msgIDs := make([]int64, len(u.Messages))
		for i, id := range u.Messages {
			msgIDs[i] = int64(id)
		}
		return curBr.OnMessageDeleted(ctx, bridge.DeleteEvent{
			ChatID:     0, // not provided by this update type
			MessageIDs: msgIDs,
		})
	})

	// OnDeleteChannelMessages — supergroup/channel deletes with ChannelID.
	dispatcher.OnDeleteChannelMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteChannelMessages) error {
		client.mu.Lock()
		curBr := client.br
		client.mu.Unlock()
		if curBr == nil {
			return nil
		}
		msgIDs := make([]int64, len(u.Messages))
		for i, id := range u.Messages {
			msgIDs[i] = int64(id)
		}
		return curBr.OnMessageDeleted(ctx, bridge.DeleteEvent{
			ChatID:     u.ChannelID,
			MessageIDs: msgIDs,
		})
	})

	// updates.Manager handles gap detection and catch_up via persisted state.
	gaps := updates.New(updates.Config{
		Handler: dispatcher,
		Storage: stateStorage,
		Logger:  nil, // gotd uses zap; we deliberately pass nil to silence it.
	})

	// Telegram client. SessionStorage is a simple file on disk; we use
	// session.FileStorage so operators can copy the .session file
	// between machines (consistent with SPEC §2.4).
	tdClient := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath},
		UpdateHandler:  gaps,
	})

	client.tdClient = tdClient
	client.dispatcher = dispatcher
	client.gaps = gaps
	return client, nil
}

// Phone returns the international phone number this Client is bound to.
func (c *Client) Phone() string {
	return c.phone
}

// API returns the raw tg.Client handle for making low-level MTProto calls
// such as peer resolution. Valid after a successful Connect.
func (c *Client) API() *tg.Client {
	return c.tdClient.API()
}

// SelfID returns the user ID of the authenticated account.
// Returns 0 before Connect has completed successfully.
func (c *Client) SelfID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selfID
}

// SetBridge replaces the active bridge. It is safe to call after Connect so
// that main can pass a bridge constructed with the real selfID.
// Subsequent dispatcher calls will pick up the new bridge atomically.
func (c *Client) SetBridge(br bridge.Bridge) {
	c.mu.Lock()
	c.br = br
	c.mu.Unlock()
}

// Connect brings the account online and starts the updates.Manager.
//
// The function blocks only until the MTProto connection is initialised,
// then returns. Background work (reconnects, update delivery, catch_up)
// runs in goroutines owned by the Client until Disconnect is called.
//
// Returns an error if the session is missing or invalid — the caller is
// expected to have run Authorize first via the --auth flow.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.stopBG != nil {
		c.mu.Unlock()
		return errors.New("tg: client is already connected")
	}
	c.mu.Unlock()

	// Derive a cancellable context tied to the client lifetime so we
	// can signal both bg.Connect and the gaps.Run goroutine on
	// Disconnect.
	runCtx, cancel := context.WithCancel(ctx)

	// bg.Connect runs telegram.Client.Run in a goroutine and blocks
	// until the client is initialised. After it returns successfully,
	// MTProto calls (including Auth().Status, Self, etc.) are usable.
	stop, err := bg.Connect(c.tdClient, bg.WithContext(runCtx))
	if err != nil {
		cancel()
		return fmt.Errorf("tg: connect %s: %w", c.phone, err)
	}

	// Verify the session is authorized — a missing or revoked session
	// must not silently spin in catch_up loops.
	status, err := c.tdClient.Auth().Status(runCtx)
	if err != nil {
		_ = stop()
		cancel()
		return fmt.Errorf("tg: auth status %s: %w", c.phone, err)
	}
	if !status.Authorized {
		_ = stop()
		cancel()
		return fmt.Errorf("tg: account %s is not authorized — run --auth first", c.phone)
	}

	self, err := c.tdClient.Self(runCtx)
	if err != nil {
		_ = stop()
		cancel()
		return fmt.Errorf("tg: self %s: %w", c.phone, err)
	}

	// Store the authenticated user ID so callers can pass it to the bridge.
	c.mu.Lock()
	c.selfID = self.ID
	c.mu.Unlock()

	// Start updates.Manager in a goroutine: it consumes raw updates
	// from the telegram client (via UpdateHandler), maintains gap
	// state in BoltDB, and dispatches typed events to our dispatcher.
	gapsErr := make(chan error, 1)
	api := c.tdClient.API()
	go func() {
		defer close(gapsErr)
		err := c.gaps.Run(runCtx, api, self.ID, updates.AuthOptions{
			OnStart: func(ctx context.Context) {
				slog.Info("updates manager started", "account", c.phone, "user_id", self.ID)
			},
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("updates manager exited", "account", c.phone, "error", err)
		}
		gapsErr <- err
	}()

	c.mu.Lock()
	c.stopBG = stop
	c.cancel = cancel
	c.gapsErr = gapsErr
	c.mu.Unlock()

	return nil
}

// Disconnect cancels the run context and waits for the background
// goroutines to exit, bounded by a 10 second timeout.
//
// Calling Disconnect on a never-connected Client is a no-op so the
// caller can defer it unconditionally.
func (c *Client) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	stop := c.stopBG
	cancel := c.cancel
	gapsErr := c.gapsErr
	c.stopBG = nil
	c.cancel = nil
	c.gapsErr = nil
	c.mu.Unlock()

	if stop == nil {
		// Never connected, but we still need to release the BoltDB
		// handle so callers can re-create the Client cleanly.
		return c.closeBolt()
	}

	// Tell the run context to wind down.
	cancel()

	// Bound the shutdown by both the caller's context and a 10s cap.
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 10*time.Second)
	defer timeoutCancel()

	// Wait for bg.Connect's goroutine to finish.
	stopDone := make(chan error, 1)
	go func() { stopDone <- stop() }()

	var firstErr error
	select {
	case err := <-stopDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			firstErr = fmt.Errorf("tg: stop %s: %w", c.phone, err)
		}
	case <-timeoutCtx.Done():
		firstErr = fmt.Errorf("tg: stop %s timed out: %w", c.phone, timeoutCtx.Err())
	}

	// Wait (briefly) for gaps.Run to drain.
	if gapsErr != nil {
		select {
		case <-gapsErr:
		case <-timeoutCtx.Done():
			// Already accounted for in firstErr above.
		}
	}

	if err := c.closeBolt(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Authorize runs the interactive auth flow used by `userbot --auth`.
//
// If the session at sessionPath is already valid, Authorize returns nil
// without prompting. Otherwise it asks the operator for the SMS code
// (and optionally the 2FA password) using the supplied callbacks.
//
// Authorize takes full control of the telegram.Client.Run lifecycle and
// returns once the auth flow completes. The Client must NOT be
// Connect()ed before calling Authorize and should be discarded after.
func (c *Client) Authorize(ctx context.Context, codePrompt CodePrompt, passwordPrompt PasswordPrompt) error {
	if codePrompt == nil {
		return errors.New("tg: codePrompt must not be nil")
	}

	var authErr error
	runErr := c.tdClient.Run(ctx, func(ctx context.Context) error {
		status, err := c.tdClient.Auth().Status(ctx)
		if err != nil {
			authErr = fmt.Errorf("tg: auth status: %w", err)
			return authErr
		}
		if status.Authorized {
			slog.Info("session already valid", "account", c.phone)
			return nil
		}

		// auth.Constant supplies phone+password+code via the auth.Flow
		// machinery. The password is fetched lazily via the prompt only
		// if Telegram actually requires 2FA.
		userAuth := lazyUserAuth{
			phone:        c.phone,
			passwordFunc: passwordPrompt,
			codeFunc:     codePrompt,
		}

		flow := auth.NewFlow(userAuth, auth.SendCodeOptions{})
		if err := c.tdClient.Auth().IfNecessary(ctx, flow); err != nil {
			authErr = fmt.Errorf("tg: auth flow: %w", err)
			return authErr
		}

		self, err := c.tdClient.Self(ctx)
		if err != nil {
			authErr = fmt.Errorf("tg: self after auth: %w", err)
			return authErr
		}
		slog.Info("auth completed",
			"account", c.phone,
			"user_id", self.ID,
			"username", self.Username,
		)
		return nil
	})
	if runErr != nil && authErr == nil {
		return fmt.Errorf("tg: client.Run during auth: %w", runErr)
	}
	if authErr != nil {
		return authErr
	}
	// Release BoltDB so subsequent runs (without --auth) can reopen it.
	return c.closeBolt()
}

// closeBolt closes the BoltDB handle once. Safe to call multiple times.
func (c *Client) closeBolt() error {
	c.mu.Lock()
	db := c.boltDB
	c.boltDB = nil
	c.mu.Unlock()

	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("tg: close bolt: %w", err)
	}
	return nil
}

// handleAlbumMessage buffers a grouped message and schedules a flush.
// Called from the OnNewMessage dispatcher whenever msg.GroupedID != 0.
func (c *Client) handleAlbumMessage(ctx context.Context, msg *tg.Message, br bridge.Bridge) {
	gid := msg.GroupedID

	newBuf := &albumBuffer{
		chatID:  peerID(msg.PeerID),
		fromID:  fromPeerID(msg.FromID),
		groupID: gid,
	}
	val, _ := c.albums.LoadOrStore(gid, newBuf)
	ab := val.(*albumBuffer)

	ab.mu.Lock()
	ab.items = append(ab.items, msg)
	n := len(ab.items)
	if ab.timer != nil {
		// Reset the quiet-period timer each time a new item arrives.
		ab.timer.Reset(500 * time.Millisecond)
	} else {
		ab.timer = time.AfterFunc(500*time.Millisecond, func() {
			c.flushAlbum(ctx, gid, br)
		})
	}
	ab.mu.Unlock()

	// Flush immediately when the Telegram album limit is reached.
	if n >= 10 {
		ab.mu.Lock()
		stopped := ab.timer.Stop()
		ab.mu.Unlock()
		if stopped {
			c.flushAlbum(ctx, gid, br)
		}
		// If Stop returned false the timer already fired and flushAlbum is
		// running (or has run) — no duplicate flush needed.
	}
}

// flushAlbum deletes the albumBuffer from the map, sorts the collected
// messages by ID, converts them to a bridge.AlbumEvent, and calls br.OnAlbum.
func (c *Client) flushAlbum(ctx context.Context, gid int64, br bridge.Bridge) {
	val, ok := c.albums.LoadAndDelete(gid)
	if !ok {
		return
	}
	ab := val.(*albumBuffer)

	ab.mu.Lock()
	msgs := make([]*tg.Message, len(ab.items))
	copy(msgs, ab.items)
	ab.mu.Unlock()

	if len(msgs) == 0 {
		return
	}

	ev := convertAlbumEvent(msgs, ab.chatID, ab.fromID, gid)
	if err := br.OnAlbum(ctx, ev); err != nil {
		slog.Error("OnAlbum failed", "grouped_id", gid, "error", err)
	}
}

// convertAlbumEvent converts a slice of raw tg.Message pointers (all sharing
// the same GroupedID) into a bridge.AlbumEvent.  Messages are sorted by ID
// ascending because Telegram may deliver them out of order.
func convertAlbumEvent(msgs []*tg.Message, chatID, fromID, gid int64) bridge.AlbumEvent {
	// Sort by message ID ascending.
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].ID < msgs[j].ID
	})

	items := make([]bridge.AlbumItem, 0, len(msgs))
	var replyToMsgID *int64

	for _, msg := range msgs {
		media := convertMedia(msg.Media)
		if media == nil {
			// Album items must have media; skip any unexpected non-media msg.
			continue
		}

		item := bridge.AlbumItem{
			MessageID: int64(msg.ID),
			Caption:   msg.Message,
			Entities:  convertEntities(msg.Entities),
			Media:     *media,
		}
		items = append(items, item)

		// Use the first item's reply header for the whole album.
		if replyToMsgID == nil && msg.ReplyTo != nil {
			if rh, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				if id, hasID := rh.GetReplyToMsgID(); hasID {
					v := int64(id)
					replyToMsgID = &v
				}
			}
		}
	}

	return bridge.AlbumEvent{
		ChatID:       chatID,
		FromID:       fromID,
		MediaGroupID: strconv.FormatInt(gid, 10),
		Items:        items,
		ReplyToMsgID: replyToMsgID,
	}
}

// peerID extracts the numeric chat id from a tg.PeerClass for logging.
func peerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return v.ChatID
	case *tg.PeerChannel:
		return v.ChannelID
	default:
		return 0
	}
}

// fromPeerID extracts a numeric sender ID from an optional PeerClass.
// Returns 0 when p is nil or an unrecognised type.
func fromPeerID(p tg.PeerClass) int64 {
	if p == nil {
		return 0
	}
	return peerID(p)
}

// lazyUserAuth implements auth.UserAuthenticator so we only invoke the
// password prompt when Telegram actually requests 2FA, and so we can
// surface our own error wrapping.
type lazyUserAuth struct {
	phone        string
	passwordFunc PasswordPrompt
	codeFunc     CodePrompt
}

func (a lazyUserAuth) Phone(ctx context.Context) (string, error) {
	return a.phone, nil
}

func (a lazyUserAuth) Password(ctx context.Context) (string, error) {
	if a.passwordFunc == nil {
		return "", errors.New("tg: 2FA password requested but no prompt configured")
	}
	return a.passwordFunc(ctx)
}

func (a lazyUserAuth) Code(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
	return a.codeFunc(ctx, sentCode)
}

func (a lazyUserAuth) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	// Userbot accounts are pre-existing; if Telegram surfaces ToS at
	// sign-in we accept silently. The operator is responsible for the
	// account.
	slog.Info("accepting Telegram ToS", "phone", a.phone)
	return nil
}

func (a lazyUserAuth) SignUp(ctx context.Context) (auth.UserInfo, error) {
	// We never sign up new accounts — this codepath is reachable only
	// for unregistered numbers. Surface a clear error so the operator
	// realises the phone is not yet a Telegram account.
	return auth.UserInfo{}, fmt.Errorf("tg: phone %s is not registered with Telegram; sign-up not supported", a.phone)
}

// convertMedia translates a tg.MessageMediaClass into a bridge.MediaRef,
// isolating the bridge package from gotd types in its public API (SPEC §3.2).
// Returns nil for unsupported or empty media.
func convertMedia(media tg.MessageMediaClass) *bridge.MediaRef {
	if media == nil {
		return nil
	}
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil
		}
		ref := &bridge.MediaRef{
			Type:       bridge.MediaTypePhoto,
			FileID:     photo.ID,
			AccessHash: photo.AccessHash,
			FileRef:    photo.FileReference,
		}
		// Pick the largest non-progressive photo size for FileSize.
		for _, sz := range photo.Sizes {
			if ps, ok := sz.(*tg.PhotoSize); ok {
				if ps.Size > int(ref.FileSize) {
					ref.FileSize = int64(ps.Size)
					ref.Width = ps.W
					ref.Height = ps.H
				}
			}
		}
		return ref

	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return nil
		}
		ref := &bridge.MediaRef{
			Type:       bridge.MediaTypeDocument,
			FileID:     doc.ID,
			AccessHash: doc.AccessHash,
			FileRef:    doc.FileReference,
			FileSize:   doc.Size,
			MimeType:   doc.MimeType,
		}
		for _, attr := range doc.Attributes {
			switch a := attr.(type) {
			case *tg.DocumentAttributeFilename:
				ref.FileName = a.FileName
			case *tg.DocumentAttributeAudio:
				if a.Voice {
					ref.Type = bridge.MediaTypeVoice
				}
				ref.Duration = a.Duration
				ref.Waveform = a.Waveform
			case *tg.DocumentAttributeVideo:
				if a.RoundMessage {
					ref.Type = bridge.MediaTypeVideoNote
					ref.IsRoundVideo = true
				} else if ref.Type == bridge.MediaTypeDocument {
					ref.Type = bridge.MediaTypeVideo
				}
				ref.Duration = int(a.Duration)
				ref.Width = a.W
				ref.Height = a.H
				ref.SupportsStreaming = a.SupportsStreaming
			case *tg.DocumentAttributeAnimated:
				if ref.Type == bridge.MediaTypeDocument {
					ref.Type = bridge.MediaTypeGIF
				}
			case *tg.DocumentAttributeSticker:
				ref.Type = bridge.MediaTypeSticker
				if a.Alt != "" {
					ref.StickerEmoji = a.Alt
				}
			case *tg.DocumentAttributeImageSize:
				ref.Width = a.W
				ref.Height = a.H
			}
		}
		return ref
	}
	return nil
}

// convertEntities translates gotd's tg.MessageEntityClass slice into the
// bridge-internal []bridge.MessageEntity, isolating the bridge package from
// gotd types in its public API (SPEC §3.2).
func convertEntities(gotdEntities []tg.MessageEntityClass) []bridge.MessageEntity {
	if len(gotdEntities) == 0 {
		return nil
	}
	result := make([]bridge.MessageEntity, 0, len(gotdEntities))
	for _, e := range gotdEntities {
		ent := bridge.MessageEntity{
			Offset: e.GetOffset(),
			Length: e.GetLength(),
		}
		switch v := e.(type) {
		case *tg.MessageEntityBold:
			ent.Type = "bold"
		case *tg.MessageEntityItalic:
			ent.Type = "italic"
		case *tg.MessageEntityUnderline:
			ent.Type = "underline"
		case *tg.MessageEntityStrike:
			ent.Type = "strike"
		case *tg.MessageEntitySpoiler:
			ent.Type = "spoiler"
		case *tg.MessageEntityCode:
			ent.Type = "code"
		case *tg.MessageEntityPre:
			ent.Type = "pre"
		case *tg.MessageEntityTextURL:
			ent.Type = "text_url"
			ent.URL = v.URL
		case *tg.MessageEntityMention:
			ent.Type = "mention"
		case *tg.MessageEntityHashtag:
			ent.Type = "hashtag"
		case *tg.MessageEntityBotCommand:
			ent.Type = "bot_command"
		default:
			ent.Type = "unknown"
		}
		result = append(result, ent)
	}
	return result
}

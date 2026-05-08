package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"golang.org/x/sync/errgroup"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// OnNewMessage is called by the tg dispatcher for every incoming message.
// Phase 4: bidirectional relay with media support.
func (b *Impl) OnNewMessage(ctx context.Context, ev MessageEvent) error {
	if !IsRelevant(ev.ChatID, ev.FromID, b.selfID, b.pairsByChat) {
		return nil
	}

	pair := b.pairsByChat[ev.ChatID]

	// Skip relay if the bot has been removed from a target chat.
	b.mu.RLock()
	_, disabled := b.disabledPairs[pair.Key]
	b.mu.RUnlock()
	if disabled {
		return nil
	}

	var direction storage.Direction
	var targetChatID int64

	switch {
	case ev.ChatID == pair.MerchantChatID:
		direction = storage.DirMerchantToSupport
		targetChatID = pair.SupportChatID
	case ev.ChatID == pair.SupportChatID:
		direction = storage.DirSupportToMerchant
		targetChatID = pair.MerchantChatID
	default:
		return nil
	}

	targetPeer, ok := b.peerByID[targetChatID]
	if !ok {
		slog.Error("no peer resolved for target chat",
			"pair_key", pair.Key,
			"target_chat_id", targetChatID,
		)
		return nil
	}

	start := time.Now()

	var replyToID int64 // 0 means "no reply"
	if ev.ReplyToMsgID != nil {
		id, found, rErr := resolveReplyTarget(ctx, b.st, ev.ChatID, *ev.ReplyToMsgID, direction)
		if rErr != nil {
			slog.Warn("failed to resolve reply target, sending as regular message",
				"pair_key", pair.Key, "error", rErr)
		} else if found {
			replyToID = id
		} else {
			slog.Warn("reply target not found, sending as regular message",
				"pair_key", pair.Key, "reply_to_msg_id", *ev.ReplyToMsgID)
		}
	}

	if err := b.humanizer.Wait(ctx); err != nil {
		return err
	}

	var updates tg.UpdatesClass
	if err := withRetry(ctx, func() error {
		var sendErr error
		updates, sendErr = b.sendMessage(ctx, targetPeer, ev, replyToID)
		return sendErr
	}); err != nil {
		if errors.Is(err, ErrChatForbidden) {
			slog.Error("bot removed from chat, disabling pair",
				"pair_key", pair.Key,
				"target_chat_id", targetChatID,
			)
			b.mu.Lock()
			b.disabledPairs[pair.Key] = struct{}{}
			b.mu.Unlock()
			return nil
		}
		slog.Error("failed to relay message",
			"pair_key", pair.Key,
			"direction", direction,
			"source_chat_id", ev.ChatID,
			"source_msg_id", ev.MessageID,
			"error", err,
		)
		return err
	}

	targetMsgID, _ := extractMessageID(updates)

	mapping := storage.MessageMapping{
		PairKey:         pair.Key,
		SourceChatID:    ev.ChatID,
		SourceMessageID: ev.MessageID,
		TargetChatID:    targetChatID,
		TargetMessageID: targetMsgID,
		Direction:       direction,
		AccountPhone:    b.accountPhone,
	}

	if err := b.st.SaveMapping(ctx, mapping); err != nil {
		if errors.Is(err, storage.ErrDuplicate) {
			slog.Debug("duplicate mapping skipped",
				"pair_key", pair.Key,
				"source_chat_id", ev.ChatID,
				"source_msg_id", ev.MessageID,
			)
			return nil
		}
		return err
	}

	slog.Info("message relayed",
		"pair_key", pair.Key,
		"direction", direction,
		"source_msg_id", ev.MessageID,
		"target_msg_id", targetMsgID,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	return nil
}

// sendMessage dispatches to the appropriate low-level send helper.
func (b *Impl) sendMessage(
	ctx context.Context,
	targetPeer tg.InputPeerClass,
	ev MessageEvent,
	replyToID int64,
) (tg.UpdatesClass, error) {
	if ev.Media != nil {
		return b.sendMedia(ctx, targetPeer, ev, replyToID)
	}
	return b.sendText(ctx, targetPeer, ev, replyToID)
}

// sendText sends a plain or entity-rich text message.
func (b *Impl) sendText(
	ctx context.Context,
	targetPeer tg.InputPeerClass,
	ev MessageEvent,
	replyToID int64,
) (tg.UpdatesClass, error) {
	entities := convertEntitiesToTG(ev.Entities)
	req := &tg.MessagesSendMessageRequest{
		Peer:     targetPeer,
		Message:  ev.Text,
		Entities: entities,
	}
	if replyToID != 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: int(replyToID)}
	}
	// RandomID is filled by the sender helper, but we call raw API directly
	// so we generate it ourselves.
	id, err := randInt64()
	if err != nil {
		return nil, fmt.Errorf("generate random_id: %w", err)
	}
	req.RandomID = id
	return b.api.MessagesSendMessage(ctx, req)
}

// sendMedia downloads the media from the source and re-uploads it to the target.
// For photos and documents that Telegram already knows about we re-send by file
// reference (no download needed).  If the file reference is stale we fall back
// to download → re-upload.
func (b *Impl) sendMedia(
	ctx context.Context,
	targetPeer tg.InputPeerClass,
	ev MessageEvent,
	replyToID int64,
) (tg.UpdatesClass, error) {
	ref := ev.Media
	reqBuilder := b.sender.To(targetPeer)
	var builder *message.Builder
	if replyToID != 0 {
		builder = reqBuilder.Reply(int(replyToID))
	} else {
		builder = &reqBuilder.Builder
	}

	switch ref.Type {
	case MediaTypePhoto:
		// Re-send by existing file location — no download.
		loc := &fileLocation{
			id:         ref.FileID,
			accessHash: ref.AccessHash,
			fileRef:    ref.FileRef,
		}
		return builder.Photo(ctx, loc)

	case MediaTypeDocument, MediaTypeGIF:
		loc := &fileLocation{
			id:         ref.FileID,
			accessHash: ref.AccessHash,
			fileRef:    ref.FileRef,
		}
		return builder.Document(ctx, loc)

	case MediaTypeVideo:
		loc := &fileLocation{
			id:         ref.FileID,
			accessHash: ref.AccessHash,
			fileRef:    ref.FileRef,
		}
		return builder.Document(ctx, loc)

	case MediaTypeVoice:
		// Voice messages must be re-uploaded for correct client rendering.
		data, err := b.downloadWithRetry(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("download voice: %w", err)
		}
		inputFile, err := b.upload(ctx, data, voiceFileName(ref), ref.MimeType)
		if err != nil {
			return nil, fmt.Errorf("upload voice: %w", err)
		}
		voiceBuilder := message.UploadedDocument(inputFile).
			MIME(mimeOrDefault(ref.MimeType, "audio/ogg")).
			Voice()
		voiceBuilder.Duration(time.Duration(ref.Duration) * time.Second)
		if len(ref.Waveform) > 0 {
			voiceBuilder.Waveform(ref.Waveform)
		}
		return builder.Media(ctx, voiceBuilder)

	case MediaTypeVideoNote:
		// Round videos must be re-uploaded.
		data, err := b.downloadWithRetry(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("download video_note: %w", err)
		}
		inputFile, err := b.upload(ctx, data, "video_note.mp4", ref.MimeType)
		if err != nil {
			return nil, fmt.Errorf("upload video_note: %w", err)
		}
		vidBuilder := message.UploadedDocument(inputFile).
			MIME(mimeOrDefault(ref.MimeType, "video/mp4")).
			RoundVideo()
		vidBuilder.Duration(time.Duration(ref.Duration) * time.Second)
		if ref.Width > 0 && ref.Height > 0 {
			vidBuilder.Resolution(ref.Width, ref.Height)
		}
		return builder.Media(ctx, vidBuilder)

	case MediaTypeSticker:
		loc := &fileLocation{
			id:         ref.FileID,
			accessHash: ref.AccessHash,
			fileRef:    ref.FileRef,
		}
		return builder.Document(ctx, loc)

	default:
		// Unknown type — forward as generic document by file reference.
		loc := &fileLocation{
			id:         ref.FileID,
			accessHash: ref.AccessHash,
			fileRef:    ref.FileRef,
		}
		return builder.Document(ctx, loc)
	}
}

// downloadWithRetry downloads the media bytes, retrying up to 3 times with
// exponential back-off (1 s, 2 s, 4 s).
func (b *Impl) downloadWithRetry(ctx context.Context, ref *MediaRef) ([]byte, error) {
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for i := range delays {
		data, err := b.downloadMedia(ctx, ref)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if i < len(delays)-1 {
			select {
			case <-time.After(delays[i]):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("download failed after 3 attempts: %w", lastErr)
}

// downloadMedia performs a single download attempt into an in-memory buffer.
func (b *Impl) downloadMedia(ctx context.Context, ref *MediaRef) ([]byte, error) {
	dl := downloader.NewDownloader()

	var location tg.InputFileLocationClass
	switch ref.Type {
	case MediaTypePhoto:
		location = &tg.InputPhotoFileLocation{
			ID:            ref.FileID,
			AccessHash:    ref.AccessHash,
			FileReference: ref.FileRef,
			ThumbSize:     "w", // largest non-progressive size
		}
	default:
		location = &tg.InputDocumentFileLocation{
			ID:            ref.FileID,
			AccessHash:    ref.AccessHash,
			FileReference: ref.FileRef,
		}
	}

	var buf bytes.Buffer
	if _, err := dl.Download(b.api, location).Stream(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// upload uploads raw bytes as a file and returns the Telegram InputFileClass.
func (b *Impl) upload(ctx context.Context, data []byte, name, _ string) (tg.InputFileClass, error) {
	ul := uploader.NewUploader(b.api)
	upload := uploader.NewUpload(name, bytes.NewReader(data), int64(len(data)))
	return ul.Upload(ctx, upload)
}

// fileLocation is a minimal implementation of message.FileLocation
// constructed from the plain values stored in MediaRef.
type fileLocation struct {
	id         int64
	accessHash int64
	fileRef    []byte
}

func (f *fileLocation) GetID() int64            { return f.id }
func (f *fileLocation) GetAccessHash() int64    { return f.accessHash }
func (f *fileLocation) GetFileReference() []byte { return f.fileRef }

// convertEntitiesToTG converts bridge.MessageEntity back to tg.MessageEntityClass
// for sending via the raw API.
func convertEntitiesToTG(entities []MessageEntity) []tg.MessageEntityClass {
	if len(entities) == 0 {
		return nil
	}
	result := make([]tg.MessageEntityClass, 0, len(entities))
	for _, e := range entities {
		offset := e.Offset
		length := e.Length
		switch e.Type {
		case "bold":
			result = append(result, &tg.MessageEntityBold{Offset: offset, Length: length})
		case "italic":
			result = append(result, &tg.MessageEntityItalic{Offset: offset, Length: length})
		case "underline":
			result = append(result, &tg.MessageEntityUnderline{Offset: offset, Length: length})
		case "strike":
			result = append(result, &tg.MessageEntityStrike{Offset: offset, Length: length})
		case "spoiler":
			result = append(result, &tg.MessageEntitySpoiler{Offset: offset, Length: length})
		case "code":
			result = append(result, &tg.MessageEntityCode{Offset: offset, Length: length})
		case "pre":
			result = append(result, &tg.MessageEntityPre{Offset: offset, Length: length})
		case "text_url":
			result = append(result, &tg.MessageEntityTextURL{Offset: offset, Length: length, URL: e.URL})
		case "mention":
			result = append(result, &tg.MessageEntityMention{Offset: offset, Length: length})
		case "hashtag":
			result = append(result, &tg.MessageEntityHashtag{Offset: offset, Length: length})
		case "bot_command":
			result = append(result, &tg.MessageEntityBotCommand{Offset: offset, Length: length})
		// Unknown / unsupported entity types are intentionally dropped.
		}
	}
	return result
}

// extractMessageID extracts the assigned message ID from the Updates returned
// by the Telegram API after sending a message.
func extractMessageID(updates tg.UpdatesClass) (int64, bool) {
	u, ok := updates.(*tg.Updates)
	if !ok {
		return 0, false
	}
	for _, upd := range u.Updates {
		switch v := upd.(type) {
		case *tg.UpdateNewMessage:
			return int64(v.Message.GetID()), true
		case *tg.UpdateNewChannelMessage:
			return int64(v.Message.GetID()), true
		case *tg.UpdateMessageID:
			return int64(v.ID), true
		}
	}
	return 0, false
}

// voiceFileName returns a reasonable file name for a voice message.
func voiceFileName(ref *MediaRef) string {
	if ref.FileName != "" {
		return ref.FileName
	}
	return "voice.ogg"
}

// mimeOrDefault returns mime if non-empty, otherwise def.
func mimeOrDefault(mime, def string) string {
	if mime != "" {
		return mime
	}
	return def
}

// randInt64 returns a cryptographically random int64.
func randInt64() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}

// OnAlbum relays a full media group (album) to the target chat.
// Phase 5: parallel download + re-upload of all items, sent as a single
// Album call so Telegram groups them on the recipient side.
func (b *Impl) OnAlbum(ctx context.Context, ev AlbumEvent) error {
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

	var direction storage.Direction
	var targetChatID int64

	switch {
	case ev.ChatID == pair.MerchantChatID:
		direction = storage.DirMerchantToSupport
		targetChatID = pair.SupportChatID
	case ev.ChatID == pair.SupportChatID:
		direction = storage.DirSupportToMerchant
		targetChatID = pair.MerchantChatID
	default:
		return nil
	}

	targetPeer, ok := b.peerByID[targetChatID]
	if !ok {
		slog.Error("no peer resolved for target chat",
			"pair_key", pair.Key,
			"target_chat_id", targetChatID,
		)
		return nil
	}

	start := time.Now()

	// Download all media in parallel.
	inputFiles := make([]tg.InputFileClass, len(ev.Items))
	eg, dlCtx := errgroup.WithContext(ctx)
	for i, item := range ev.Items {
		i, item := i, item // capture loop vars
		eg.Go(func() error {
			ref := item.Media
			data, err := b.downloadWithRetry(dlCtx, &ref)
			if err != nil {
				return fmt.Errorf("album item %d download: %w", i, err)
			}
			name := ref.FileName
			if name == "" {
				name = albumFileName(&ref, i)
			}
			f, err := b.upload(dlCtx, data, name, ref.MimeType)
			if err != nil {
				return fmt.Errorf("album item %d upload: %w", i, err)
			}
			inputFiles[i] = f
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		slog.Error("album download/upload failed",
			"pair_key", pair.Key,
			"media_group_id", ev.MediaGroupID,
			"error", err,
		)
		return err
	}

	// Build MultiMediaOption slice.
	// The caption (if any) is attached to the first item only; subsequent
	// items are created without a caption.  Captions are passed as
	// variadic StyledTextOption args to UploadedPhoto / UploadedDocument.
	opts := make([]message.MultiMediaOption, len(ev.Items))
	for i, item := range ev.Items {
		f := inputFiles[i]
		ref := item.Media

		var captionOpts []styling.StyledTextOption
		if i == 0 && item.Caption != "" {
			captionOpts = []styling.StyledTextOption{styling.Plain(item.Caption)}
		}

		if ref.Type == MediaTypePhoto {
			opts[i] = message.UploadedPhoto(f, captionOpts...)
		} else {
			docBuilder := message.UploadedDocument(f, captionOpts...).
				MIME(mimeOrDefault(ref.MimeType, "application/octet-stream"))
			if ref.FileName != "" {
				docBuilder = docBuilder.Filename(ref.FileName)
			}
			opts[i] = docBuilder
		}
	}

	var replyToID int64 // 0 means "no reply"
	if ev.ReplyToMsgID != nil {
		id, found, rErr := resolveReplyTarget(ctx, b.st, ev.ChatID, *ev.ReplyToMsgID, direction)
		if rErr != nil {
			slog.Warn("failed to resolve reply target, sending as regular message",
				"pair_key", pair.Key, "error", rErr)
		} else if found {
			replyToID = id
		} else {
			slog.Warn("reply target not found, sending as regular message",
				"pair_key", pair.Key, "reply_to_msg_id", *ev.ReplyToMsgID)
		}
	}

	reqBuilder := b.sender.To(targetPeer)

	// Apply reply if the album was itself a reply, then send.
	var msgBuilder *message.Builder
	if replyToID != 0 {
		msgBuilder = reqBuilder.Reply(int(replyToID))
	} else {
		msgBuilder = &reqBuilder.Builder
	}

	if err := b.humanizer.Wait(ctx); err != nil {
		return err
	}

	var updates tg.UpdatesClass
	if err := withRetry(ctx, func() error {
		var sendErr error
		updates, sendErr = msgBuilder.Album(ctx, opts[0], opts[1:]...)
		return sendErr
	}); err != nil {
		if errors.Is(err, ErrChatForbidden) {
			slog.Error("bot removed from chat, disabling pair",
				"pair_key", pair.Key,
				"target_chat_id", targetChatID,
			)
			b.mu.Lock()
			b.disabledPairs[pair.Key] = struct{}{}
			b.mu.Unlock()
			return nil
		}
		slog.Error("failed to relay album",
			"pair_key", pair.Key,
			"direction", direction,
			"media_group_id", ev.MediaGroupID,
			"error", err,
		)
		return err
	}

	targetIDs := extractAlbumMessageIDs(updates)

	mgid := ev.MediaGroupID
	saved := 0
	for idx, item := range ev.Items {
		var targetMsgID int64
		if idx < len(targetIDs) {
			targetMsgID = targetIDs[idx]
		}
		mapping := storage.MessageMapping{
			PairKey:         pair.Key,
			SourceChatID:    ev.ChatID,
			SourceMessageID: item.MessageID,
			TargetChatID:    targetChatID,
			TargetMessageID: targetMsgID,
			Direction:       direction,
			AccountPhone:    b.accountPhone,
			MediaGroupID:    &mgid,
		}
		if err := b.st.SaveMapping(ctx, mapping); err != nil {
			if errors.Is(err, storage.ErrDuplicate) {
				slog.Debug("duplicate album mapping skipped",
					"pair_key", pair.Key,
					"source_msg_id", item.MessageID,
				)
				continue
			}
			slog.Error("save album mapping failed",
				"pair_key", pair.Key,
				"source_msg_id", item.MessageID,
				"error", err,
			)
		} else {
			saved++
		}
	}

	if len(targetIDs) < len(ev.Items) {
		slog.Warn("album partial relay",
			"pair_key", pair.Key,
			"media_group_id", ev.MediaGroupID,
			"sent_items", len(ev.Items),
			"extracted_ids", len(targetIDs),
		)
	}

	slog.Info("album relayed",
		"pair_key", pair.Key,
		"direction", direction,
		"media_group_id", ev.MediaGroupID,
		"count", len(ev.Items),
		"duration_ms", time.Since(start).Milliseconds(),
	)

	return nil
}

// extractAlbumMessageIDs pulls all message IDs from an Album send result.
// The Album method returns a single tg.UpdatesClass that may contain multiple
// UpdateNewMessage / UpdateNewChannelMessage / UpdateMessageID updates — one
// per album item.
func extractAlbumMessageIDs(u tg.UpdatesClass) []int64 {
	upds, ok := u.(*tg.Updates)
	if !ok {
		return nil
	}
	ids := make([]int64, 0, len(upds.Updates))
	for _, upd := range upds.Updates {
		switch v := upd.(type) {
		case *tg.UpdateNewMessage:
			ids = append(ids, int64(v.Message.GetID()))
		case *tg.UpdateNewChannelMessage:
			ids = append(ids, int64(v.Message.GetID()))
		case *tg.UpdateMessageID:
			ids = append(ids, int64(v.ID))
		}
	}
	return ids
}

// albumFileName returns a fallback file name for an album item that has no
// FileName, based on its media type and index.
func albumFileName(ref *MediaRef, idx int) string {
	switch ref.Type {
	case MediaTypePhoto:
		return fmt.Sprintf("photo_%d.jpg", idx)
	case MediaTypeVideo:
		return fmt.Sprintf("video_%d.mp4", idx)
	case MediaTypeGIF:
		return fmt.Sprintf("animation_%d.mp4", idx)
	default:
		return fmt.Sprintf("file_%d", idx)
	}
}


package bridge

// MessageEntity is a styled-text span in a message body.
// It abstracts gotd's tg.MessageEntityClass so the bridge package
// has no compile-time dependency on gotd library types in its public API.
type MessageEntity struct {
	Offset int
	Length int
	// Type is the entity kind: "bold", "italic", "code", "pre",
	// "text_url", "mention", "hashtag", "spoiler", etc.
	Type string
	// URL is set for "text_url" entities only.
	URL string
}

// MediaType discriminates the Telegram media kind for relay.
type MediaType string

const (
	MediaTypePhoto     MediaType = "photo"
	MediaTypeVideo     MediaType = "video"
	MediaTypeDocument  MediaType = "document"
	MediaTypeVoice     MediaType = "voice"
	MediaTypeVideoNote MediaType = "video_note"
	MediaTypeSticker   MediaType = "sticker"
	MediaTypeGIF       MediaType = "gif"
)

// MediaRef contains all information needed to re-send a media item.
// No gotd types here — SPEC §3.2.
type MediaRef struct {
	Type MediaType

	// File identity coordinates (common for photos and documents).
	FileID     int64
	AccessHash int64
	FileRef    []byte
	FileSize   int64 // bytes; 0 if unknown

	// Upload metadata.
	MimeType string
	FileName string // for documents

	// Dimensional / temporal metadata.
	Width    int
	Height   int
	Duration int    // seconds; for video/voice/video_note
	Waveform []byte // for voice notes only

	// Video flags.
	SupportsStreaming bool
	IsRoundVideo     bool // true for VideoNote

	// Sticker / animation flags.
	IsAnimated   bool // true for TGS stickers
	IsVideo      bool // true for WEBM video stickers
	StickerEmoji string
}

// MessageEvent carries normalised fields from an incoming NewMessage update.
type MessageEvent struct {
	ChatID       int64
	MessageID    int64
	FromID       int64
	Text         string
	Entities     []MessageEntity
	ReplyToMsgID *int64
	Media        *MediaRef // nil for text-only messages
}

// EditEvent carries the data for a message-edited update.
type EditEvent struct {
	ChatID    int64
	MessageID int64
	// FromID is the sender of the original message; used by IsRelevant to
	// filter out edits made by the userbot itself (loop prevention).
	FromID   int64
	Text     string
	Entities []MessageEntity
	// NewMedia is set when the edit also changed the attached media.
	// Nil means text/caption-only edit.
	NewMedia *MediaRef
}

// DeleteEvent carries the data for a message-deleted update (Phase 5).
type DeleteEvent struct {
	ChatID     int64
	MessageIDs []int64
}

// ReactionsEvent carries the data for a message-reactions update (Phase 7).
type ReactionsEvent struct {
	ChatID    int64
	MessageID int64
}

// AlbumItem is one element of a media group (album).
// Albums always contain media; Caption is set only on the first item in
// practice, but the field is present on every item for completeness.
type AlbumItem struct {
	MessageID int64
	// Caption is the text/caption of this album element (usually only the
	// first item has one).
	Caption  string
	Entities []MessageEntity
	Media    MediaRef // albums always have media
}

// AlbumEvent is fired when a full album (media group) is ready.
// No gotd types — SPEC §3.2.
type AlbumEvent struct {
	ChatID int64
	FromID int64
	// MediaGroupID is the Telegram grouped_id expressed as a decimal string.
	MediaGroupID string
	// Items contains 2–10 elements, sorted ascending by MessageID.
	Items        []AlbumItem
	ReplyToMsgID *int64 // set if the album is a reply
}

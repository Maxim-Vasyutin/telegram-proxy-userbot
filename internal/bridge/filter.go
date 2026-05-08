package bridge

// Pair holds the routing information for one merchant↔support pair.
//
// When looked up by MerchantChatID, TargetChatID == SupportChatID.
// When looked up by SupportChatID,  TargetChatID == MerchantChatID.
// This dual-entry layout is built by New so routing is a single map lookup.
type Pair struct {
	// Key is the human-readable identifier from config (e.g. "merch_1").
	Key string
	// MerchantChatID is the Telegram ID of the merchant group.
	MerchantChatID int64
	// SupportChatID is the Telegram ID of the support group.
	SupportChatID int64
	// TargetChatID is pre-computed as the "other side" of the pair:
	// SupportChatID when looked up via MerchantChatID, and vice-versa.
	TargetChatID int64
}

// IsRelevant returns true when the incoming message should be processed by
// the bridge. It enforces two invariants from SPEC §3.5:
//
//  1. The chat must be one of the configured pairs.
//  2. The sender must not be the userbot account itself (selfID), to avoid
//     relay loops.
func IsRelevant(chatID int64, fromID int64, selfID int64, pairs map[int64]Pair) bool {
	_, ok := pairs[chatID]
	if !ok {
		return false
	}
	if fromID == selfID {
		return false
	}
	return true
}

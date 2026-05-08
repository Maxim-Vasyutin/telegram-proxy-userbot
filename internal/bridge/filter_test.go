package bridge

import "testing"

// testPairs returns a minimal pairs map for use in filter tests.
func testPairs() map[int64]Pair {
	return map[int64]Pair{
		-1001000000001: {
			Key:            "pair_1",
			MerchantChatID: -1001000000001,
			SupportChatID:  -1001000000002,
			TargetChatID:   -1001000000002,
		},
		-1001000000002: {
			Key:            "pair_1",
			MerchantChatID: -1001000000001,
			SupportChatID:  -1001000000002,
			TargetChatID:   -1001000000001,
		},
	}
}

func TestIsRelevant_relevant(t *testing.T) {
	pairs := testPairs()
	const (
		chatID = int64(-1001000000001)
		fromID = int64(999)
		selfID = int64(111)
	)
	if !IsRelevant(chatID, fromID, selfID, pairs) {
		t.Error("expected IsRelevant to return true for a known chat with a different sender")
	}
}

func TestIsRelevant_notInPairs(t *testing.T) {
	pairs := testPairs()
	const (
		chatID = int64(-1001999999999) // not in pairs
		fromID = int64(999)
		selfID = int64(111)
	)
	if IsRelevant(chatID, fromID, selfID, pairs) {
		t.Error("expected IsRelevant to return false for an unknown chat")
	}
}

func TestIsRelevant_fromSelf(t *testing.T) {
	pairs := testPairs()
	const (
		chatID = int64(-1001000000001)
		selfID = int64(111)
	)
	// fromID == selfID — the userbot sent this message itself.
	if IsRelevant(chatID, selfID, selfID, pairs) {
		t.Error("expected IsRelevant to return false when fromID == selfID")
	}
}

func TestIsRelevant_zeroChatID(t *testing.T) {
	pairs := testPairs()
	const (
		chatID = int64(0)
		fromID = int64(999)
		selfID = int64(111)
	)
	if IsRelevant(chatID, fromID, selfID, pairs) {
		t.Error("expected IsRelevant to return false for chatID 0")
	}
}

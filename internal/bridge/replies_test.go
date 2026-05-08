package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// mockStorage is an in-test implementation of storage.Storage shared across
// bridge unit tests (replies_test.go, edits_test.go).
type mockStorage struct {
	byTarget     map[[2]int64]*storage.MessageMapping
	bySource     map[[2]int64]*storage.MessageMapping
	targetErr    error
	sourceErr    error
	// findBySourceFn is an optional observer invoked inside FindBySource before
	// the normal lookup, used in edits_test.go to verify call arguments.
	findBySourceFn func(chatID, messageID int64)
}

func (m *mockStorage) FindByTarget(ctx context.Context, chatID, messageID int64) (*storage.MessageMapping, error) {
	if m.targetErr != nil {
		return nil, m.targetErr
	}
	mm := m.byTarget[[2]int64{chatID, messageID}]
	return mm, nil
}

func (m *mockStorage) FindBySource(ctx context.Context, chatID, messageID int64) (*storage.MessageMapping, error) {
	if m.findBySourceFn != nil {
		m.findBySourceFn(chatID, messageID)
	}
	if m.sourceErr != nil {
		return nil, m.sourceErr
	}
	mm := m.bySource[[2]int64{chatID, messageID}]
	return mm, nil
}

func (m *mockStorage) SaveMapping(_ context.Context, _ storage.MessageMapping) error { return nil }
func (m *mockStorage) FindByMediaGroup(_ context.Context, _ string) ([]storage.MessageMapping, error) {
	return nil, nil
}
func (m *mockStorage) Ping(_ context.Context) error { return nil }
func (m *mockStorage) Close() error                 { return nil }

// TestResolveReplyTarget_foundViaTarget verifies that when FindByTarget returns a
// mapping, the function returns the source message ID with found=true.
func TestResolveReplyTarget_foundViaTarget(t *testing.T) {
	st := &mockStorage{
		byTarget: map[[2]int64]*storage.MessageMapping{
			{100, 42}: {
				SourceChatID:    200,
				SourceMessageID: 99,
				TargetChatID:    100,
				TargetMessageID: 42,
			},
		},
	}

	id, found, err := resolveReplyTarget(context.Background(), st, 100, 42, storage.DirSupportToMerchant)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if id != 99 {
		t.Fatalf("expected targetReplyID=99, got %d", id)
	}
}

// TestResolveReplyTarget_foundViaSource verifies that when FindByTarget returns nil
// but FindBySource returns a mapping, the function returns the target message ID.
func TestResolveReplyTarget_foundViaSource(t *testing.T) {
	st := &mockStorage{
		byTarget: map[[2]int64]*storage.MessageMapping{}, // no match
		bySource: map[[2]int64]*storage.MessageMapping{
			{100, 55}: {
				SourceChatID:    100,
				SourceMessageID: 55,
				TargetChatID:    200,
				TargetMessageID: 77,
			},
		},
	}

	id, found, err := resolveReplyTarget(context.Background(), st, 100, 55, storage.DirMerchantToSupport)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true, got false")
	}
	if id != 77 {
		t.Fatalf("expected targetReplyID=77, got %d", id)
	}
}

// TestResolveReplyTarget_notFound verifies that when both lookups return nil,
// the function returns found=false and no error.
func TestResolveReplyTarget_notFound(t *testing.T) {
	st := &mockStorage{
		byTarget: map[[2]int64]*storage.MessageMapping{},
		bySource: map[[2]int64]*storage.MessageMapping{},
	}

	id, found, err := resolveReplyTarget(context.Background(), st, 100, 1, storage.DirMerchantToSupport)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false, got true")
	}
	if id != 0 {
		t.Fatalf("expected targetReplyID=0, got %d", id)
	}
}

// TestResolveReplyTarget_storageError verifies that a FindByTarget storage error
// is propagated immediately without calling FindBySource.
func TestResolveReplyTarget_storageError(t *testing.T) {
	sentinel := errors.New("db connection lost")
	st := &mockStorage{
		targetErr: sentinel,
		// sourceErr intentionally unset — should never be reached
	}

	_, _, err := resolveReplyTarget(context.Background(), st, 100, 1, storage.DirMerchantToSupport)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got: %v", err)
	}
}

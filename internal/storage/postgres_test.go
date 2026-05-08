package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	. "github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
)

// testDSN is set by TestMain once the container is running.
var testDSN string

// TestMain starts a PostgreSQL container, applies migrations, runs all tests,
// and terminates the container on exit.
func TestMain(m *testing.M) {
	ctx := context.Background()

	pgCtr, err := tcpostgres.Run(ctx,
		"postgres:15-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testcontainers: start postgres: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if terr := testcontainers.TerminateContainer(pgCtr); terr != nil {
			fmt.Fprintf(os.Stderr, "testcontainers: terminate: %v\n", terr)
		}
	}()

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testcontainers: connection string: %v\n", err)
		os.Exit(1)
	}
	testDSN = dsn

	// Apply migrations via goose using database/sql + pgx stdlib driver.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		fmt.Fprintf(os.Stderr, "goose.SetDialect: %v\n", err)
		os.Exit(1)
	}

	if err := goose.Up(db, "../../migrations"); err != nil {
		fmt.Fprintf(os.Stderr, "goose.Up: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// newStorage is a helper that builds a Postgres storage pointed at the
// container started in TestMain.
func newStorage(t *testing.T) Storage {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, testDSN, 5, 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sampleMapping returns a MessageMapping with unique IDs derived from base so
// that multiple test cases don't collide.
func sampleMapping(base int64) MessageMapping {
	return MessageMapping{
		PairKey:         "merch_1",
		SourceChatID:    -1001000000000 - base,
		SourceMessageID: base,
		TargetChatID:    -1002000000000 - base,
		TargetMessageID: base + 1000,
		Direction:       DirMerchantToSupport,
		MediaGroupID:    nil,
		AccountPhone:    "+79001234567",
	}
}

// TestSaveMapping_creates verifies that a valid mapping is persisted and can be
// retrieved by source coordinates.
func TestSaveMapping_creates(t *testing.T) {
	s := newStorage(t)
	ctx := context.Background()

	m := sampleMapping(1)
	if err := s.SaveMapping(ctx, m); err != nil {
		t.Fatalf("SaveMapping: %v", err)
	}

	got, err := s.FindBySource(ctx, m.SourceChatID, m.SourceMessageID)
	if err != nil {
		t.Fatalf("FindBySource: %v", err)
	}
	if got == nil {
		t.Fatal("FindBySource returned nil, want a record")
	}
	if got.PairKey != m.PairKey {
		t.Errorf("PairKey: got %q, want %q", got.PairKey, m.PairKey)
	}
	if got.TargetChatID != m.TargetChatID {
		t.Errorf("TargetChatID: got %d, want %d", got.TargetChatID, m.TargetChatID)
	}
	if got.Direction != m.Direction {
		t.Errorf("Direction: got %q, want %q", got.Direction, m.Direction)
	}
}

// TestSaveMapping_duplicateReturnsErrDuplicate verifies that inserting a mapping
// with the same (source_chat_id, source_message_id) returns ErrDuplicate.
func TestSaveMapping_duplicateReturnsErrDuplicate(t *testing.T) {
	s := newStorage(t)
	ctx := context.Background()

	m := sampleMapping(2)
	if err := s.SaveMapping(ctx, m); err != nil {
		t.Fatalf("first SaveMapping: %v", err)
	}

	err := s.SaveMapping(ctx, m)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second SaveMapping: got %v, want ErrDuplicate", err)
	}
}

// TestFindBySource_returnsMapping verifies that FindBySource locates the correct
// record and returns nil when no record exists.
func TestFindBySource_returnsMapping(t *testing.T) {
	s := newStorage(t)
	ctx := context.Background()

	m := sampleMapping(3)
	if err := s.SaveMapping(ctx, m); err != nil {
		t.Fatalf("SaveMapping: %v", err)
	}

	got, err := s.FindBySource(ctx, m.SourceChatID, m.SourceMessageID)
	if err != nil {
		t.Fatalf("FindBySource (found): %v", err)
	}
	if got == nil {
		t.Fatal("FindBySource returned nil for existing record")
	}
	if got.SourceMessageID != m.SourceMessageID {
		t.Errorf("SourceMessageID: got %d, want %d", got.SourceMessageID, m.SourceMessageID)
	}

	// Non-existent coordinates must yield (nil, nil).
	missing, err := s.FindBySource(ctx, -999999999, 999999)
	if err != nil {
		t.Fatalf("FindBySource (missing): %v", err)
	}
	if missing != nil {
		t.Errorf("FindBySource (missing): got %+v, want nil", missing)
	}
}

// TestFindByTarget_returnsMapping verifies that FindByTarget resolves a relayed
// message back to its origin record.
func TestFindByTarget_returnsMapping(t *testing.T) {
	s := newStorage(t)
	ctx := context.Background()

	m := sampleMapping(4)
	if err := s.SaveMapping(ctx, m); err != nil {
		t.Fatalf("SaveMapping: %v", err)
	}

	got, err := s.FindByTarget(ctx, m.TargetChatID, m.TargetMessageID)
	if err != nil {
		t.Fatalf("FindByTarget (found): %v", err)
	}
	if got == nil {
		t.Fatal("FindByTarget returned nil for existing record")
	}
	if got.TargetMessageID != m.TargetMessageID {
		t.Errorf("TargetMessageID: got %d, want %d", got.TargetMessageID, m.TargetMessageID)
	}

	// Non-existent coordinates must yield (nil, nil).
	missing, err := s.FindByTarget(ctx, -999999999, 999999)
	if err != nil {
		t.Fatalf("FindByTarget (missing): %v", err)
	}
	if missing != nil {
		t.Errorf("FindByTarget (missing): got %+v, want nil", missing)
	}
}

// TestFindByMediaGroup_returnsAlbum verifies that all album members sharing the
// same media_group_id are returned together.
func TestFindByMediaGroup_returnsAlbum(t *testing.T) {
	s := newStorage(t)
	ctx := context.Background()

	groupID := "album-group-42"

	// Insert three album members.
	for i := int64(0); i < 3; i++ {
		m := sampleMapping(500 + i)
		m.MediaGroupID = &groupID
		if err := s.SaveMapping(ctx, m); err != nil {
			t.Fatalf("SaveMapping[%d]: %v", i, err)
		}
	}

	results, err := s.FindByMediaGroup(ctx, groupID)
	if err != nil {
		t.Fatalf("FindByMediaGroup: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("FindByMediaGroup: got %d records, want 3", len(results))
	}
	for _, r := range results {
		if r.MediaGroupID == nil || *r.MediaGroupID != groupID {
			t.Errorf("unexpected media_group_id: %v", r.MediaGroupID)
		}
	}

	// Non-existent group must return an empty (non-nil) slice.
	empty, err := s.FindByMediaGroup(ctx, "no-such-group")
	if err != nil {
		t.Fatalf("FindByMediaGroup (empty): %v", err)
	}
	if empty == nil {
		t.Error("FindByMediaGroup returned nil slice, want empty non-nil slice")
	}
	if len(empty) != 0 {
		t.Errorf("FindByMediaGroup (empty): got %d records, want 0", len(empty))
	}
}

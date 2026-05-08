package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// mockController is a test double for AccountController.
type mockController struct {
	phone         string
	connectCalled atomic.Int32
	disconnCalled atomic.Int32
	connectErr    error
}

func (m *mockController) Connect(_ context.Context) error {
	m.connectCalled.Add(1)
	return m.connectErr
}
func (m *mockController) Disconnect(_ context.Context) error {
	m.disconnCalled.Add(1)
	return nil
}
func (m *mockController) Phone() string { return m.phone }

// mskLoc is the Europe/Moscow timezone used throughout tests.
var mskLoc = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		panic(err)
	}
	return loc
}()

// ---------- activeShift table tests ----------

func TestActiveShift_DayShift_InRange(t *testing.T) {
	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, mskLoc) // 10:00 MSK — A active
	got, next := activeShift(now, shifts)
	if got.AccountPhone != "A" {
		t.Fatalf("want A at 10:00, got %s", got.AccountPhone)
	}
	wantNext := time.Date(2026, 1, 1, 20, 0, 0, 0, mskLoc)
	if !next.Equal(wantNext) {
		t.Fatalf("want nextSwitch %v, got %v", wantNext, next)
	}
}

func TestActiveShift_DayShift_ExactBoundary(t *testing.T) {
	// 08:00 exactly — [08:00, 20:00) includes the start.
	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}
	now := time.Date(2026, 1, 1, 8, 0, 0, 0, mskLoc)
	got, _ := activeShift(now, shifts)
	if got.AccountPhone != "A" {
		t.Fatalf("want A at 08:00 boundary, got %s", got.AccountPhone)
	}
}

func TestActiveShift_NightShift_PreMidnight(t *testing.T) {
	// 23:00 — overnight shift [20:00, 08:00) active in its pre-midnight half.
	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, mskLoc)
	got, next := activeShift(now, shifts)
	if got.AccountPhone != "B" {
		t.Fatalf("want B at 23:00, got %s", got.AccountPhone)
	}
	// Switch happens next day at 08:00.
	wantNext := time.Date(2026, 1, 2, 8, 0, 0, 0, mskLoc)
	if !next.Equal(wantNext) {
		t.Fatalf("want nextSwitch %v, got %v", wantNext, next)
	}
}

func TestActiveShift_NightShift_PostMidnight(t *testing.T) {
	// 02:00 — overnight shift still active after midnight.
	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}
	now := time.Date(2026, 1, 1, 2, 0, 0, 0, mskLoc)
	got, next := activeShift(now, shifts)
	if got.AccountPhone != "B" {
		t.Fatalf("want B at 02:00, got %s", got.AccountPhone)
	}
	// Switch is today at 08:00 (still in the same day's morning).
	wantNext := time.Date(2026, 1, 1, 8, 0, 0, 0, mskLoc)
	if !next.Equal(wantNext) {
		t.Fatalf("want nextSwitch %v, got %v", wantNext, next)
	}
}

func TestActiveShift_SingleShift_FullDay(t *testing.T) {
	// A single shift spanning most of the day — any time within range returns it.
	shifts := []Shift{
		{AccountPhone: "A", From: 0, To: 23*time.Hour + 59*time.Minute},
	}
	now := time.Date(2026, 1, 1, 15, 30, 0, 0, mskLoc)
	got, _ := activeShift(now, shifts)
	if got.AccountPhone != "A" {
		t.Fatalf("want A, got %s", got.AccountPhone)
	}
}

// ---------- Scheduler.Run tests ----------

func TestScheduler_Run_SingleAccount_24_7(t *testing.T) {
	ctrl := &mockController{phone: "+71111111111"}
	sched := &impl{
		shifts:      nil, // empty = 24/7 mode
		controllers: map[string]AccountController{"+71111111111": ctrl},
		tz:          mskLoc,
		nowFn:       time.Now,
		switchDelay: 0,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Give the goroutine time to call Connect before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := ctrl.connectCalled.Load(); n != 1 {
		t.Fatalf("expected Connect called 1 time, got %d", n)
	}
	if n := ctrl.disconnCalled.Load(); n != 1 {
		t.Fatalf("expected Disconnect called 1 time, got %d", n)
	}
}

func TestScheduler_Run_TwoAccounts_ActiveSelected(t *testing.T) {
	ctrlA := &mockController{phone: "A"}
	ctrlB := &mockController{phone: "B"}

	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}
	// Fixed time: 20:01 → B is the active shift.
	// Use a date well in the future so nextSwitch (next-day 08:00) is not in the past,
	// which would cause time.Until to be negative and fire the timer immediately.
	fixedNow := time.Date(2030, 6, 1, 20, 1, 0, 0, mskLoc)

	ctx, cancel := context.WithCancel(context.Background())
	sched := &impl{
		shifts:      shifts,
		controllers: map[string]AccountController{"A": ctrlA, "B": ctrlB},
		tz:          mskLoc,
		nowFn:       func() time.Time { return fixedNow },
		switchDelay: 0,
	}

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Give the goroutine time to connect B.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	// Only B should have been connected.
	if n := ctrlB.connectCalled.Load(); n != 1 {
		t.Fatalf("expected B.Connect = 1, got %d", n)
	}
	if n := ctrlA.connectCalled.Load(); n != 0 {
		t.Fatalf("expected A.Connect = 0, got %d", n)
	}
	if n := ctrlB.disconnCalled.Load(); n != 1 {
		t.Fatalf("expected B.Disconnect = 1, got %d", n)
	}
}

func TestScheduler_Run_TwoAccounts_SwitchOrder(t *testing.T) {
	ctrlA := &mockController{phone: "A"}
	ctrlB := &mockController{phone: "B"}

	shifts := []Shift{
		{AccountPhone: "A", From: 8 * time.Hour, To: 20 * time.Hour},
		{AccountPhone: "B", From: 20 * time.Hour, To: 8 * time.Hour},
	}

	// First call: 1 ms before A→B switch using a past date so nextSwitch
	// (2026-01-01 20:00) is already in the past → time.Until is negative →
	// time.After fires immediately, triggering the switch right away.
	// Subsequent calls: a future date so B's nextSwitch is far ahead and the
	// timer does not fire again before the test cancels.
	callCount := 0
	ctx, cancel := context.WithCancel(context.Background())
	sched := &impl{
		shifts:      shifts,
		controllers: map[string]AccountController{"A": ctrlA, "B": ctrlB},
		tz:          mskLoc,
		switchDelay: 0,
		nowFn: func() time.Time {
			callCount++
			if callCount == 1 {
				// A is active; nextSwitch = 2026-01-01 20:00 (already in the past).
				return time.Date(2026, 1, 1, 19, 59, 59, 0, mskLoc)
			}
			// After switch: B is active, nextSwitch = 2030-06-02 08:00 (far future).
			return time.Date(2030, 6, 1, 21, 0, 0, 0, mskLoc)
		},
	}

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Wait for the switch cycle: A connects, switch fires (~1 ms), A disconnects,
	// B connects. Cancel after that.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if n := ctrlA.connectCalled.Load(); n != 1 {
		t.Errorf("A.Connect want 1, got %d", n)
	}
	if n := ctrlA.disconnCalled.Load(); n != 1 {
		t.Errorf("A.Disconnect want 1, got %d", n)
	}
	if n := ctrlB.connectCalled.Load(); n != 1 {
		t.Errorf("B.Connect want 1, got %d", n)
	}
	if n := ctrlB.disconnCalled.Load(); n != 1 {
		t.Errorf("B.Disconnect want 1, got %d", n)
	}
}

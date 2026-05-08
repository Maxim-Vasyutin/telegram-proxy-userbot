// Package scheduler implements account-shift switching for the userbot.
// A Scheduler connects the active account (determined by MSK-timezone shifts)
// and switches to the next account when a shift boundary is crossed.
package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// Shift maps a time window to an account phone number.
// From and To are durations from midnight in the scheduler's timezone.
// If From > To the window wraps through midnight (e.g. 20h→8h covers
// 20:00–23:59 and 00:00–08:00).
type Shift struct {
	AccountPhone string
	From         time.Duration // e.g. 8 * time.Hour
	To           time.Duration // e.g. 20 * time.Hour
}

// AccountController manages the lifecycle of a single Telegram account.
type AccountController interface {
	// Connect brings the account online. Idempotent.
	Connect(ctx context.Context) error
	// Disconnect tears the account down without LogOut.
	Disconnect(ctx context.Context) error
	// Phone returns the account identifier used as map key.
	Phone() string
}

// Scheduler switches accounts according to configured shift windows.
type Scheduler interface {
	// Run blocks until ctx is cancelled.
	// On start it connects the account active at that moment.
	// With multiple accounts it switches accounts on shift boundaries.
	Run(ctx context.Context) error
}

type impl struct {
	shifts      []Shift
	controllers map[string]AccountController
	tz          *time.Location

	// nowFn is used by tests to inject a fixed clock; nil means time.Now.
	nowFn func() time.Time
	// switchDelay is the pause between Disconnect and Connect during a
	// shift switch. Set to 1 s in production, 0 in unit tests.
	switchDelay time.Duration
}

// New constructs a Scheduler.
//
// Pass an empty shifts slice for single-account 24/7 operation.
// With two accounts, shifts must fully cover 24 hours (validated by config).
func New(shifts []Shift, controllers map[string]AccountController, tz *time.Location) Scheduler {
	return &impl{
		shifts:      shifts,
		controllers: controllers,
		tz:          tz,
		nowFn:       time.Now,
		switchDelay: time.Second,
	}
}

func (s *impl) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// Run implements Scheduler.
func (s *impl) Run(ctx context.Context) error {
	if len(s.shifts) == 0 {
		return s.run247(ctx)
	}
	return s.runShifted(ctx)
}

// run247 connects the single controller and waits for ctx cancellation.
func (s *impl) run247(ctx context.Context) error {
	ctrl := s.singleController()
	if err := ctrl.Connect(ctx); err != nil {
		return err
	}
	slog.Info("telegram connected (24/7 mode)", "account", ctrl.Phone())

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ctrl.Disconnect(shutdownCtx); err != nil {
		slog.Error("disconnect failed on shutdown", "account", ctrl.Phone(), "error", err)
	}
	slog.Info("telegram disconnected", "account", ctrl.Phone())
	return ctx.Err()
}

// runShifted runs the shift-switching loop.
func (s *impl) runShifted(ctx context.Context) error {
	for {
		active, nextSwitch := activeShift(s.now().In(s.tz), s.shifts)

		ctrl, ok := s.controllers[active.AccountPhone]
		if !ok {
			slog.Error("scheduler: no controller for active account",
				"account", active.AccountPhone)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(60 * time.Second):
				continue
			}
		}

		if err := ctrl.Connect(ctx); err != nil {
			slog.Error("scheduler: connect failed, retrying in 60s",
				"account", active.AccountPhone, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(60 * time.Second):
				continue
			}
		}
		slog.Info("telegram connected", "account", ctrl.Phone(), "next_switch", nextSwitch.Format(time.RFC3339))

		// Wait until the shift boundary or shutdown.
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = ctrl.Disconnect(shutdownCtx)
			slog.Info("telegram disconnected", "account", ctrl.Phone())
			return ctx.Err()
		case <-time.After(time.Until(nextSwitch)):
		}

		// Shift boundary crossed: hand off to the next account.
		slog.Info("shift switch start", "from_account", ctrl.Phone())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ctrl.Disconnect(shutdownCtx); err != nil {
			slog.Error("disconnect during shift switch failed",
				"account", ctrl.Phone(), "error", err)
		}
		cancel()

		if s.switchDelay > 0 {
			time.Sleep(s.switchDelay)
		}
		slog.Info("shift switch complete")
	}
}

// singleController returns the one controller available for 24/7 mode.
func (s *impl) singleController() AccountController {
	for _, c := range s.controllers {
		return c
	}
	panic("scheduler: no controllers configured")
}

// activeShift returns the Shift that is active at now and the absolute time
// when the next shift boundary will be crossed.
// now must already be in the scheduler's timezone.
func activeShift(now time.Time, shifts []Shift) (Shift, time.Time) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	current := now.Sub(midnight)

	for _, sh := range shifts {
		var active bool
		if sh.From < sh.To {
			// Normal range e.g. [08:00, 20:00).
			active = current >= sh.From && current < sh.To
		} else {
			// Overnight range e.g. [20:00, 08:00) — wraps through midnight.
			active = current >= sh.From || current < sh.To
		}
		if !active {
			continue
		}

		var nextSwitch time.Time
		if sh.From < sh.To {
			nextSwitch = midnight.Add(sh.To)
		} else {
			if current >= sh.From {
				// In the pre-midnight half: switch at next day's midnight + To.
				nextSwitch = midnight.Add(24*time.Hour + sh.To)
			} else {
				// In the post-midnight half: switch at today's midnight + To.
				nextSwitch = midnight.Add(sh.To)
			}
		}
		return sh, nextSwitch
	}

	// No active shift found (should not happen with a valid config).
	// Fall back to the first shift's next start time.
	first := shifts[0]
	nextSwitch := midnight.Add(first.From)
	if !now.Before(nextSwitch) {
		nextSwitch = nextSwitch.Add(24 * time.Hour)
	}
	return first, nextSwitch
}

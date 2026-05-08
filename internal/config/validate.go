package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ValidateConfig performs fail-fast validation of a parsed Config.
// It returns the first error found; the caller should treat any error
// as fatal (exit code 2).
func ValidateConfig(cfg *Config) error {
	if err := validateAccounts(cfg); err != nil {
		return err
	}
	if err := validateSchedule(cfg); err != nil {
		return err
	}
	if err := validatePairs(cfg); err != nil {
		return err
	}
	if err := validateHumanization(cfg); err != nil {
		return err
	}
	if err := validateEnvVars(cfg); err != nil {
		return err
	}
	return nil
}

// validateAccounts checks account count, uniqueness, and non-empty phones.
func validateAccounts(cfg *Config) error {
	count := len(cfg.Accounts)
	if count < 1 || count > 2 {
		return fmt.Errorf("config: accounts must contain 1 or 2 entries, got %d", count)
	}

	seen := make(map[string]struct{}, count)
	for i, acc := range cfg.Accounts {
		if strings.TrimSpace(acc.Phone) == "" {
			return fmt.Errorf("config: accounts[%d].phone must not be empty", i)
		}
		if _, dup := seen[acc.Phone]; dup {
			return fmt.Errorf("config: accounts[%d].phone %q is a duplicate", i, acc.Phone)
		}
		seen[acc.Phone] = struct{}{}
	}
	return nil
}

// validateSchedule checks the timezone and shift coverage.
func validateSchedule(cfg *Config) error {
	if strings.TrimSpace(cfg.Schedule.Timezone) == "" {
		return fmt.Errorf("config: schedule.timezone must not be empty")
	}
	loc, err := time.LoadLocation(cfg.Schedule.Timezone)
	if err != nil {
		return fmt.Errorf("config: schedule.timezone %q is invalid: %w", cfg.Schedule.Timezone, err)
	}
	_ = loc

	// With one account shifts are optional (24/7 mode).
	if len(cfg.Accounts) == 1 {
		return nil
	}

	// Two accounts require fully-covering, non-overlapping shifts.
	if len(cfg.Schedule.Shifts) == 0 {
		return fmt.Errorf("config: schedule.shifts must not be empty when 2 accounts are configured")
	}

	// Parse and validate each shift.
	type interval struct {
		phone    string
		fromMins int
		toMins   int
	}
	intervals := make([]interval, 0, len(cfg.Schedule.Shifts))

	phoneSet := make(map[string]struct{}, len(cfg.Accounts))
	for _, acc := range cfg.Accounts {
		phoneSet[acc.Phone] = struct{}{}
	}

	for i, s := range cfg.Schedule.Shifts {
		if _, ok := phoneSet[s.AccountPhone]; !ok {
			return fmt.Errorf("config: schedule.shifts[%d].account_phone %q does not match any account", i, s.AccountPhone)
		}
		fromMins, err := parseHHMM(s.From)
		if err != nil {
			return fmt.Errorf("config: schedule.shifts[%d].from %q: %w", i, s.From, err)
		}
		toMins, err := parseHHMM(s.To)
		if err != nil {
			return fmt.Errorf("config: schedule.shifts[%d].to %q: %w", i, s.To, err)
		}
		intervals = append(intervals, interval{phone: s.AccountPhone, fromMins: fromMins, toMins: toMins})
	}

	// Check for overlaps by marking each minute of the day.
	// Minutes covered: [from, to) with midnight wraparound.
	const day = 24 * 60
	coverage := make([]int, day) // 0 = uncovered

	for idx, iv := range intervals {
		mark := idx + 1
		if iv.fromMins == iv.toMins {
			return fmt.Errorf("config: schedule.shifts[%d]: from and to are equal (%s)", idx, cfg.Schedule.Shifts[idx].From)
		}
		if iv.fromMins < iv.toMins {
			// Normal range: e.g. 08:00 → 20:00
			for m := iv.fromMins; m < iv.toMins; m++ {
				if coverage[m] != 0 {
					return fmt.Errorf("config: schedule.shifts[%d] overlaps with shifts[%d] at minute %d", idx, coverage[m]-1, m)
				}
				coverage[m] = mark
			}
		} else {
			// Overnight range: e.g. 20:00 → 08:00
			for m := iv.fromMins; m < day; m++ {
				if coverage[m] != 0 {
					return fmt.Errorf("config: schedule.shifts[%d] overlaps with shifts[%d] at minute %d", idx, coverage[m]-1, m)
				}
				coverage[m] = mark
			}
			for m := 0; m < iv.toMins; m++ {
				if coverage[m] != 0 {
					return fmt.Errorf("config: schedule.shifts[%d] overlaps with shifts[%d] at minute %d", idx, coverage[m]-1, m)
				}
				coverage[m] = mark
			}
		}
	}

	// Ensure full 24-hour coverage.
	for m := 0; m < day; m++ {
		if coverage[m] == 0 {
			h := m / 60
			min := m % 60
			return fmt.Errorf("config: schedule.shifts do not cover %02d:%02d — 24-hour coverage required", h, min)
		}
	}

	return nil
}

// parseHHMM converts "HH:MM" to total minutes since midnight.
func parseHHMM(s string) (int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM format")
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("invalid hour %q", parts[0])
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid minute %q", parts[1])
	}
	return h*60 + m, nil
}

// validatePairs checks pair count, uniqueness, and non-empty IDs.
func validatePairs(cfg *Config) error {
	if len(cfg.Pairs) < 1 {
		return fmt.Errorf("config: pairs must contain at least 1 entry")
	}

	seenKeys := make(map[string]struct{}, len(cfg.Pairs))
	seenChatIDs := make(map[int64]string, len(cfg.Pairs)*2)

	for i, p := range cfg.Pairs {
		if strings.TrimSpace(p.Key) == "" {
			return fmt.Errorf("config: pairs[%d].key must not be empty", i)
		}
		if _, dup := seenKeys[p.Key]; dup {
			return fmt.Errorf("config: pairs[%d].key %q is a duplicate", i, p.Key)
		}
		seenKeys[p.Key] = struct{}{}

		if p.MerchantChatID == 0 {
			return fmt.Errorf("config: pairs[%d].merchant_chat_id must not be zero", i)
		}
		if p.SupportChatID == 0 {
			return fmt.Errorf("config: pairs[%d].support_chat_id must not be zero", i)
		}
		if p.MerchantChatID == p.SupportChatID {
			return fmt.Errorf("config: pairs[%d]: merchant_chat_id and support_chat_id must differ", i)
		}

		// Telegram group IDs are negative; warn but do not fatal — validated in logs by main.
		// Here we only enforce uniqueness across all pairs.
		if prev, dup := seenChatIDs[p.MerchantChatID]; dup {
			return fmt.Errorf("config: pairs[%d].merchant_chat_id %d is already used in pair %q", i, p.MerchantChatID, prev)
		}
		seenChatIDs[p.MerchantChatID] = p.Key

		if prev, dup := seenChatIDs[p.SupportChatID]; dup {
			return fmt.Errorf("config: pairs[%d].support_chat_id %d is already used in pair %q", i, p.SupportChatID, prev)
		}
		seenChatIDs[p.SupportChatID] = p.Key
	}
	return nil
}

// validateHumanization checks that min <= max delay.
func validateHumanization(cfg *Config) error {
	h := cfg.Humanization
	if h.SendDelayMinMs < 0 {
		return fmt.Errorf("config: humanization.send_delay_min_ms must be >= 0, got %d", h.SendDelayMinMs)
	}
	if h.SendDelayMaxMs < h.SendDelayMinMs {
		return fmt.Errorf("config: humanization.send_delay_max_ms (%d) must be >= send_delay_min_ms (%d)", h.SendDelayMaxMs, h.SendDelayMinMs)
	}
	return nil
}

// validateEnvVars checks that required environment variables are present.
func validateEnvVars(cfg *Config) error {
	apiIDEnv := cfg.Telegram.APIIDEnv
	if apiIDEnv == "" {
		apiIDEnv = "TELEGRAM_API_ID"
	}
	if os.Getenv(apiIDEnv) == "" {
		return fmt.Errorf("config: required environment variable %q is not set (telegram.api_id_env)", apiIDEnv)
	}

	apiHashEnv := cfg.Telegram.APIHashEnv
	if apiHashEnv == "" {
		apiHashEnv = "TELEGRAM_API_HASH"
	}
	if os.Getenv(apiHashEnv) == "" {
		return fmt.Errorf("config: required environment variable %q is not set (telegram.api_hash_env)", apiHashEnv)
	}

	dsnEnv := cfg.Postgres.DSNEnv
	if dsnEnv == "" {
		dsnEnv = "DATABASE_DSN"
	}
	if os.Getenv(dsnEnv) == "" {
		return fmt.Errorf("config: required environment variable %q is not set (postgres.dsn_env)", dsnEnv)
	}

	return nil
}

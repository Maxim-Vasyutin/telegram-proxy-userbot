// Package config provides YAML config parsing and startup validation
// for the telegram-proxy-userbot.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure.
// It mirrors the layout of config.yaml exactly.
type Config struct {
	Telegram      TelegramConfig      `yaml:"telegram"`
	Accounts      []AccountConfig     `yaml:"accounts"`
	Schedule      ScheduleConfig      `yaml:"schedule"`
	Pairs         []PairConfig        `yaml:"pairs"`
	Humanization  HumanizationConfig  `yaml:"humanization"`
	Postgres      PostgresConfig      `yaml:"postgres"`
	Logging       LoggingConfig       `yaml:"logging"`
	State         StateConfig         `yaml:"state"`
}

// TelegramConfig holds references to environment variables
// that contain the Telegram API credentials. The actual values
// are never stored in the YAML file.
type TelegramConfig struct {
	// APIIDEnv is the name of the env variable holding TELEGRAM_API_ID.
	APIIDEnv string `yaml:"api_id_env"`
	// APIHashEnv is the name of the env variable holding TELEGRAM_API_HASH.
	APIHashEnv string `yaml:"api_hash_env"`
}

// AccountConfig describes a single Telegram userbot account.
type AccountConfig struct {
	// Phone is the phone number in international format, e.g. "+79001234567".
	Phone string `yaml:"phone"`
	// SessionPath is the absolute path to the .session file on disk.
	SessionPath string `yaml:"session_path"`
}

// ScheduleConfig defines timezone and account shift boundaries.
type ScheduleConfig struct {
	// Timezone is a valid IANA tz database name, e.g. "Europe/Moscow".
	Timezone string `yaml:"timezone"`
	// Shifts lists the time ranges for each account.
	// Must cover 24 hours without overlaps when 2 accounts are configured.
	Shifts []ShiftConfig `yaml:"shifts"`
}

// ShiftConfig maps a single time window to an account phone number.
type ShiftConfig struct {
	// AccountPhone must match one of the phones in Accounts.
	AccountPhone string `yaml:"account_phone"`
	// From is the shift start in "HH:MM" 24-hour format.
	From string `yaml:"from"`
	// To is the shift end in "HH:MM" 24-hour format.
	To string `yaml:"to"`
}

// PairConfig defines a one-to-one mapping between a merchant group
// and a support group.
type PairConfig struct {
	// Key is a unique human-readable identifier used in logs and DB records.
	Key string `yaml:"key"`
	// MerchantChatID is the Telegram ID of the merchant group (negative).
	MerchantChatID int64 `yaml:"merchant_chat_id"`
	// SupportChatID is the Telegram ID of the support group (negative).
	SupportChatID int64 `yaml:"support_chat_id"`
}

// HumanizationConfig controls the random delay injected before each relay.
type HumanizationConfig struct {
	// SendDelayMinMs is the lower bound of the uniform delay in milliseconds.
	SendDelayMinMs int `yaml:"send_delay_min_ms"`
	// SendDelayMaxMs is the upper bound of the uniform delay in milliseconds.
	SendDelayMaxMs int `yaml:"send_delay_max_ms"`
}

// PostgresConfig holds PostgreSQL pool settings.
// The DSN is read from an environment variable, never stored in YAML.
type PostgresConfig struct {
	// DSNEnv is the name of the env variable holding the PostgreSQL DSN.
	DSNEnv string `yaml:"dsn_env"`
	// MaxConns is the maximum number of connections in the pool.
	MaxConns int32 `yaml:"max_conns"`
	// MinConns is the minimum number of idle connections kept in the pool.
	MinConns int32 `yaml:"min_conns"`
}

// LoggingConfig controls log level and output format.
type LoggingConfig struct {
	// Level is one of: debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is one of: json, text.
	Format string `yaml:"format"`
}

// StateConfig points to the BoltDB file used by gotd for updates state.
type StateConfig struct {
	// Path is the absolute path to the .bolt state file.
	Path string `yaml:"path"`
}

// LoadConfig reads the YAML file at path and decodes it into a Config.
// Unknown fields cause a parse error (strict mode via KnownFields).
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %q: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode %q: %w", path, err)
	}

	return &cfg, nil
}

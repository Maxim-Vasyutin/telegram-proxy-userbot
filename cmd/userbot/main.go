// Command userbot is the entry point for the Telegram Proxy Userbot.
//
// Supported flags:
//
//	--version   Print version and exit 0.
//	--check     Load and validate config, exit 0 on success or exit 2 on error.
//	--auth      Authorize a Telegram account interactively (requires --phone).
//	--phone     Phone number used with --auth.
//	--config    Path to config.yaml (default: ./config.yaml).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/scheduler"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/tg"

	tgproto "github.com/gotd/td/tg"
)

const version = "0.1.0-dev"

const defaultConfigPath = "config.yaml"

func main() {
	// -------------------------------------------------------------------------
	// Flags
	// -------------------------------------------------------------------------
	versionFlag := flag.Bool("version", false, "print version and exit")
	checkFlag := flag.Bool("check", false, "validate config and exit (0 = OK, 2 = invalid)")
	authFlag := flag.Bool("auth", false, "interactive Telegram account authorization (requires --phone)")
	phoneFlag := flag.String("phone", "", "phone number for --auth, e.g. +79001234567")
	configPath := flag.String("config", defaultConfigPath, "path to config.yaml")
	flag.Parse()

	// -------------------------------------------------------------------------
	// --version
	// -------------------------------------------------------------------------
	if *versionFlag {
		fmt.Println(version)
		os.Exit(0)
	}

	// -------------------------------------------------------------------------
	// JSON logger — used for all remaining paths
	// -------------------------------------------------------------------------
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// -------------------------------------------------------------------------
	// Load and validate config (shared by --check, --auth, and normal run)
	// -------------------------------------------------------------------------
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err, "path", *configPath)
		os.Exit(2)
	}

	if err := config.ValidateConfig(cfg); err != nil {
		slog.Error("config validation failed", "error", err)
		os.Exit(2)
	}

	// Per SPEC §4.3: warn if any chat_id is positive (Telegram group IDs are negative).
	for i, p := range cfg.Pairs {
		if p.MerchantChatID > 0 {
			slog.Warn("pairs[i].merchant_chat_id is positive — expected a negative group ID",
				"index", i, "key", p.Key, "chat_id", p.MerchantChatID)
		}
		if p.SupportChatID > 0 {
			slog.Warn("pairs[i].support_chat_id is positive — expected a negative group ID",
				"index", i, "key", p.Key, "chat_id", p.SupportChatID)
		}
	}

	// -------------------------------------------------------------------------
	// --check: exit after validation
	// -------------------------------------------------------------------------
	if *checkFlag {
		slog.Info("config is valid",
			"accounts", len(cfg.Accounts),
			"pairs", len(cfg.Pairs),
		)
		os.Exit(0)
	}

	// -------------------------------------------------------------------------
	// --auth: interactive authorization for a single account
	// -------------------------------------------------------------------------
	if *authFlag {
		if err := runAuth(cfg, *phoneFlag); err != nil {
			slog.Error("auth failed", "error", err, "phone", *phoneFlag)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// -------------------------------------------------------------------------
	// Normal run: log startup info, then wait for shutdown signal
	// -------------------------------------------------------------------------
	slog.Info("config loaded",
		"version", version,
		"pid", os.Getpid(),
		"accounts_count", len(cfg.Accounts),
		"pairs_count", len(cfg.Pairs),
		"config_path", *configPath,
	)

	// Create a context that is cancelled on SIGINT or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initialise storage (PostgreSQL connection pool).
	store, err := storage.New(ctx, os.Getenv(cfg.Postgres.DSNEnv), cfg.Postgres.MaxConns, cfg.Postgres.MinConns)
	if err != nil {
		slog.Error("failed to connect to storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	slog.Info("storage connected", "max_conns", cfg.Postgres.MaxConns)

	// Resolve API credentials.
	apiID, apiHash, err := readAPICreds(cfg)
	if err != nil {
		slog.Error("failed to read Telegram API credentials", "error", err)
		os.Exit(1)
	}

	// Create one tg.Client + ClientController per account.
	// Each account gets its own BoltDB state file derived from the base path.
	controllers := make(map[string]scheduler.AccountController, len(cfg.Accounts))
	for _, acc := range cfg.Accounts {
		boltPath := boltPathForAccount(cfg.State.Path, acc.Phone)
		client, err := tg.New(acc, apiID, apiHash, boltPath, nil)
		if err != nil {
			slog.Error("failed to create telegram client",
				"error", err, "account", acc.Phone)
			os.Exit(1)
		}
		controllers[acc.Phone] = scheduler.NewClientController(client, store, cfg.Pairs)
	}

	// Load timezone and convert config shifts to scheduler.Shift values.
	tz, err := time.LoadLocation(cfg.Schedule.Timezone)
	if err != nil {
		// Already validated; this should never happen.
		slog.Error("invalid timezone", "error", err)
		os.Exit(1)
	}
	shifts := configShiftsToScheduler(cfg.Schedule.Shifts)

	sched := scheduler.New(shifts, controllers, tz)

	slog.Info("userbot starting")
	if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("scheduler exited with error", "error", err)
	}
	slog.Info("userbot stopped")
}

// runAuth performs the interactive --auth flow for a single account.
func runAuth(cfg *config.Config, phone string) error {
	if strings.TrimSpace(phone) == "" {
		return errors.New("--auth requires --phone <number>")
	}

	var account *config.AccountConfig
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Phone == phone {
			account = &cfg.Accounts[i]
			break
		}
	}
	if account == nil {
		return fmt.Errorf("phone %q not found in config.accounts", phone)
	}

	apiID, apiHash, err := readAPICreds(cfg)
	if err != nil {
		return err
	}

	boltPath := boltPathForAccount(cfg.State.Path, account.Phone)
	tgClient, err := tg.New(*account, apiID, apiHash, boltPath, nil)
	if err != nil {
		return fmt.Errorf("create telegram client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	codePrompt := func(_ context.Context, _ *tgproto.AuthSentCode) (string, error) {
		fmt.Fprintf(os.Stderr, "Enter the login code Telegram sent to %s: ", phone)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read code: %w", err)
		}
		return strings.TrimSpace(line), nil
	}

	passwordPrompt := func(_ context.Context) (string, error) {
		fmt.Fprintf(os.Stderr, "Enter the cloud (2FA) password for %s: ", phone)
		fd := int(os.Stdin.Fd())
		if term.IsTerminal(fd) {
			pw, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", fmt.Errorf("read password: %w", err)
			}
			return strings.TrimSpace(string(pw)), nil
		}
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(line), nil
	}

	return tgClient.Authorize(ctx, codePrompt, passwordPrompt)
}

// readAPICreds resolves TELEGRAM_API_ID and TELEGRAM_API_HASH from the
// environment using the variable names declared in the config.
func readAPICreds(cfg *config.Config) (int, string, error) {
	apiIDEnv := cfg.Telegram.APIIDEnv
	if apiIDEnv == "" {
		apiIDEnv = "TELEGRAM_API_ID"
	}
	apiHashEnv := cfg.Telegram.APIHashEnv
	if apiHashEnv == "" {
		apiHashEnv = "TELEGRAM_API_HASH"
	}

	apiIDStr := os.Getenv(apiIDEnv)
	apiHash := os.Getenv(apiHashEnv)
	if apiIDStr == "" || apiHash == "" {
		return 0, "", fmt.Errorf("env vars %s/%s must be set", apiIDEnv, apiHashEnv)
	}
	apiID, err := strconv.Atoi(apiIDStr)
	if err != nil {
		return 0, "", fmt.Errorf("env %s is not a valid integer: %w", apiIDEnv, err)
	}
	return apiID, apiHash, nil
}

// boltPathForAccount derives a per-account BoltDB path from the base path.
// The phone number is sanitised to digits only before being appended so
// the resulting filename is safe on all platforms.
func boltPathForAccount(basePath, phone string) string {
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, phone)
	base := strings.TrimSuffix(basePath, ".bolt")
	return base + "_" + digits + ".bolt"
}

// configShiftsToScheduler converts config-level ShiftConfig (HH:MM strings)
// to scheduler.Shift (durations from midnight).
func configShiftsToScheduler(cfgShifts []config.ShiftConfig) []scheduler.Shift {
	result := make([]scheduler.Shift, 0, len(cfgShifts))
	for _, s := range cfgShifts {
		result = append(result, scheduler.Shift{
			AccountPhone: s.AccountPhone,
			From:         hhmmToDuration(s.From),
			To:           hhmmToDuration(s.To),
		})
	}
	return result
}

// hhmmToDuration converts a validated "HH:MM" string to a duration from midnight.
// The config validator guarantees the format is correct before this is called.
func hhmmToDuration(hhmm string) time.Duration {
	t, _ := time.Parse("15:04", hhmm)
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

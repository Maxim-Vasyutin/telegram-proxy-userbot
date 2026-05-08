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

	"golang.org/x/term"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/bridge"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/tg"

	"github.com/gotd/td/telegram/message"
	tgproto "github.com/gotd/td/tg"
)

const version = "0.1.0-dev"

// configPath is the default location of the YAML config file inside the container.
// Override at runtime by setting CONFIG_PATH or using the --config flag (future).
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

	// Resolve API credentials. validateEnvVars already ensured these
	// are present, but we still need to convert API_ID to int.
	apiID, apiHash, err := readAPICreds(cfg)
	if err != nil {
		slog.Error("failed to read Telegram API credentials", "error", err)
		os.Exit(1)
	}

	// Phase 2 has no scheduler yet — connect the first account from the
	// config and let it run until SIGTERM. Phase 9 will replace this
	// with a real Scheduler.
	if len(cfg.Accounts) == 0 {
		slog.Error("no accounts configured")
		os.Exit(1)
	}
	primary := cfg.Accounts[0]

	// Create the tg client without a bridge initially. The bridge requires
	// selfID which is only known after Connect. SetBridge wires it in safely.
	tgClient, err := tg.New(primary, apiID, apiHash, cfg.State.Path, nil)
	if err != nil {
		slog.Error("failed to create telegram client", "error", err, "account", primary.Phone)
		os.Exit(1)
	}

	if err := tgClient.Connect(ctx); err != nil {
		slog.Error("failed to connect telegram", "error", err, "account", primary.Phone)
		os.Exit(1)
	}
	slog.Info("telegram connected", "account", tgClient.Phone())

	// Build sender and bridge now that we have a connected client and selfID.
	sender := message.NewSender(tgClient.API())
	br := bridge.New(store, sender, tgClient.API(), cfg.Pairs, tgClient.SelfID(), primary.Phone)

	// Resolve access hashes for all pair chat IDs before relaying starts.
	if err := br.ResolvePeers(ctx, tgClient.API()); err != nil {
		slog.Error("failed to resolve peers", "error", err)
		os.Exit(1)
	}

	// Wire the bridge into the dispatcher. Any update arriving after this
	// point will be forwarded to the bridge for relay.
	tgClient.SetBridge(br)

	slog.Info("userbot running — waiting for shutdown signal")

	<-ctx.Done()

	slog.Info("shutdown signal received, exiting", "signal", ctx.Err())

	// Disconnect with a bounded timeout — we use Background here
	// because the root ctx is already cancelled.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := tgClient.Disconnect(shutdownCtx); err != nil {
		slog.Error("telegram disconnect", "error", err, "account", tgClient.Phone())
	} else {
		slog.Info("telegram disconnected", "account", tgClient.Phone())
	}
}

// runAuth performs the interactive --auth flow for a single account.
// Returns nil on success, or an error on validation/auth failure.
func runAuth(cfg *config.Config, phone string) error {
	if strings.TrimSpace(phone) == "" {
		return errors.New("--auth requires --phone <number>")
	}

	// Find the account in config.
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

	tgClient, err := tg.New(*account, apiID, apiHash, cfg.State.Path, nil)
	if err != nil {
		return fmt.Errorf("create telegram client: %w", err)
	}

	// Cancel auth on SIGINT/SIGTERM so the operator can abort cleanly.
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
		// Read with masking if stdin is a TTY; otherwise fall back to
		// plain input so that piping (CI / scripted runs) still works.
		fd := int(os.Stdin.Fd())
		if term.IsTerminal(fd) {
			pw, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr) // newline after masked input
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

	if err := tgClient.Authorize(ctx, codePrompt, passwordPrompt); err != nil {
		return err
	}
	return nil
}

// readAPICreds resolves TELEGRAM_API_ID and TELEGRAM_API_HASH from the
// environment using the variable names declared in the config.
// validateEnvVars has already ensured the variables are non-empty, so
// this only validates that API_ID is a valid integer.
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

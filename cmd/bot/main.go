// Command bot polls the Uma Musume API hourly for master.mdb updates, sends
// table and campaign_data diffs to a Telegram chat, and optionally serves HTTP
// webhook endpoints for Telegram and Discord.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"kudapan_lokal/internal/discordbot"
	"kudapan_lokal/internal/masterdb"
	"kudapan_lokal/internal/notify"
	"kudapan_lokal/internal/savedata"
	"kudapan_lokal/internal/umaclient"
	"kudapan_lokal/internal/watcher"
	"kudapan_lokal/internal/webhook"
)

const (
	defaultEnvPath     = ".env"
	defaultLogFilePath = "data/bot.log"
)

type stdoutNotifier struct{}

func (stdoutNotifier) SendMessage(_ context.Context, text string) error {
	fmt.Println("----- TELEGRAM MESSAGE -----")
	fmt.Println(text)
	fmt.Println("----------------------------")
	return nil
}

func (stdoutNotifier) SendMessageToChat(_ context.Context, chatID, text string) error {
	fmt.Printf("----- TELEGRAM MESSAGE (chat_id=%s) -----\n", chatID)
	fmt.Println(text)
	fmt.Println("-----------------------------------------")
	return nil
}

type stdoutDiscordSender struct{}

func (stdoutDiscordSender) SendChannelMessage(_ context.Context, channelID, content, roleID string) error {
	fmt.Printf("----- DISCORD MESSAGE (channel_id=%s, role_id=%s) -----\n", channelID, roleID)
	fmt.Println(content)
	fmt.Println("---------------------------------------------------------")
	return nil
}

type webhookConfig struct {
	ListenAddr            string
	TelegramWebhookSecret string
	DiscordPublicKey      string
	DiscordAppID          string
	DiscordBotToken       string
	DiscordDBPath         string
}

func resolveFlagOrEnv(flagVal, envKey string) string {
	if trimmed := strings.TrimSpace(flagVal); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(os.Getenv(envKey))
}

func buildWebhookServer(
	cfg webhookConfig,
	sender webhook.ChatSender,
	dcStore *discordbot.Store,
	dcVerifier discordbot.PermissionVerifier,
	masterDBPath string,
	logger *log.Logger,
) (*webhook.Server, error) {
	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		return nil, nil
	}
	if logger == nil {
		logger = log.Default()
	}

	tgHandler := webhook.NewTelegramHandler(webhook.TelegramConfig{
		SecretToken: cfg.TelegramWebhookSecret,
		Sender:      sender,
		Logger:      logger,
	})

	var dcHandler http.Handler
	if strings.TrimSpace(cfg.DiscordPublicKey) != "" {
		h, err := webhook.NewDiscordHandler(webhook.DiscordConfig{
			PublicKeyHex: cfg.DiscordPublicKey,
			Logger:       logger,
		})
		if err != nil {
			return nil, err
		}
		if dcStore != nil {
			discordbot.RegisterCommands(h, dcStore, dcVerifier, masterDBPath, time.Now)
		}
		dcHandler = h
	} else {
		logger.Printf("discord webhook disabled (/api/webhook/discord returns 503): DISCORD_PUBLIC_KEY not set")
	}

	if cfg.DiscordAppID != "" || cfg.DiscordBotToken != "" {
		logger.Printf("discord app metadata loaded (app_id_set=%v, bot_token_set=%v)",
			cfg.DiscordAppID != "", cfg.DiscordBotToken != "")
	}

	return webhook.NewServer(webhook.Config{
		ListenAddr:      listenAddr,
		TelegramHandler: tgHandler,
		DiscordHandler:  dcHandler,
		Logger:          logger,
	})
}

func main() {
	var (
		envPath               = flag.String("env", defaultEnvPath, "Path to .env file")
		token                 = flag.String("token", "", "Telegram bot token (overrides TELEGRAM_BOT_TOKEN in .env/env)")
		chatID                = flag.String("chat-id", "", "Telegram chat ID (overrides TELEGRAM_CHAT_ID in .env/env)")
		listenAddr            = flag.String("listen", "", "HTTP listen address for webhook server (e.g. :8080; empty disables webhook server; overrides WEBHOOK_LISTEN_ADDR)")
		telegramWebhookSecret = flag.String("telegram-webhook-secret", "", "Optional secret token for /api/webhook/telegram (overrides TELEGRAM_WEBHOOK_SECRET)")
		discordPublicKey      = flag.String("discord-public-key", "", "Discord application Ed25519 public key (hex) for /api/webhook/discord (overrides DISCORD_PUBLIC_KEY)")
		discordAppID          = flag.String("discord-app-id", "", "Optional Discord application ID (overrides DISCORD_APP_ID)")
		discordBotToken       = flag.String("discord-bot-token", "", "Optional Discord bot token (overrides DISCORD_BOT_TOKEN)")
		discordDBPath         = flag.String("discord-db", "", "Path to Discord bot SQLite database (defaults to data/discord.db; overrides DISCORD_DB_PATH)")
		savedataPath          = flag.String("savedata", savedata.DefaultFilePath, "Path to savedata.json")
		statePath             = flag.String("state", watcher.DefaultStatePath, "Path to watcher state.json")
		dbPath                = flag.String("db", watcher.DefaultMasterDBPath, "Path to cached baseline master.mdb")
		seedDBPath            = flag.String("seed-db", "", "Optional path to seed initial baseline master.mdb (e.g. /mnt/bind/master.mdb)")
		interval              = flag.Duration("interval", watcher.DefaultInterval, "Check interval (e.g. 1h)")
		once                  = flag.Bool("once", false, "Run a single check and exit")
		dryRun                = flag.Bool("dry-run", false, "Print formatted messages to stdout instead of sending to Telegram")
		logFilePath           = flag.String("log-file", defaultLogFilePath, "Path to append logs alongside console output (empty disables file logging)")
		verbose               = flag.Bool("verbose", false, "Enable verbose logs (manifest resolution steps, URLs, per-table diffs)")
	)
	flag.Parse()

	logger, closeLog, err := setupLogger(*logFilePath, os.Stderr)
	if err != nil {
		log.Fatalf("init logger %q: %v", *logFilePath, err)
	}
	defer func() {
		if err := closeLog(); err != nil {
			fmt.Fprintf(os.Stderr, "close log file %q: %v\n", *logFilePath, err)
		}
	}()
	log.SetOutput(logger.Writer())
	log.SetFlags(log.LstdFlags)

	envExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "env" {
			envExplicit = true
		}
	})

	envLoaded, err := loadDotEnv(*envPath, envExplicit)
	if err != nil {
		logger.Fatalf("load env file %q: %v", *envPath, err)
	}

	resolvedToken := resolveFlagOrEnv(*token, "TELEGRAM_BOT_TOKEN")
	resolvedChatID := resolveFlagOrEnv(*chatID, "TELEGRAM_CHAT_ID")
	resolvedDiscordDB := resolveFlagOrEnv(*discordDBPath, "DISCORD_DB_PATH")
	if resolvedDiscordDB == "" {
		resolvedDiscordDB = discordbot.DefaultDBPath
	}
	whCfg := webhookConfig{
		ListenAddr:            resolveFlagOrEnv(*listenAddr, "WEBHOOK_LISTEN_ADDR"),
		TelegramWebhookSecret: resolveFlagOrEnv(*telegramWebhookSecret, "TELEGRAM_WEBHOOK_SECRET"),
		DiscordPublicKey:      resolveFlagOrEnv(*discordPublicKey, "DISCORD_PUBLIC_KEY"),
		DiscordAppID:          resolveFlagOrEnv(*discordAppID, "DISCORD_APP_ID"),
		DiscordBotToken:       resolveFlagOrEnv(*discordBotToken, "DISCORD_BOT_TOKEN"),
		DiscordDBPath:         resolvedDiscordDB,
	}

	logger.Printf(
		"starting kudapan_lokal bot (env=%q loaded=%v, savedata=%q, state=%q, db=%q, discord_db=%q, seed=%q, interval=%s, once=%v, dry_run=%v, verbose=%v, log_file=%q, webhook_listen=%q)",
		*envPath, envLoaded, *savedataPath, *statePath, *dbPath, whCfg.DiscordDBPath, *seedDBPath, *interval, *once, *dryRun, *verbose, *logFilePath, whCfg.ListenAddr,
	)

	store, err := savedata.LoadOrInit(*savedataPath)
	if err != nil {
		logger.Fatalf("load savedata: %v", err)
	}
	logger.Printf("loaded savedata (app_ver=%s, res_ver=%s, viewer_id=%s)",
		store.Data().AppVer, store.Data().ResVer, store.Data().ViewerID)

	client, err := umaclient.New(store)
	if err != nil {
		logger.Fatalf("init umaclient: %v", err)
	}

	var recvOpts []masterdb.Option
	if *verbose {
		recvOpts = append(recvOpts, masterdb.WithLogger(logger))
	}
	recv, err := masterdb.New(store, client, recvOpts...)
	if err != nil {
		logger.Fatalf("init masterdb receiver: %v", err)
	}

	var (
		notifier   notify.Notifier
		chatSender webhook.ChatSender
	)
	if *dryRun {
		logger.Printf("dry-run mode enabled: formatted notifications will print to stdout instead of Telegram")
		dry := stdoutNotifier{}
		notifier = dry
		chatSender = dry
	} else {
		tg, err := notify.NewTelegramClient(resolvedToken, resolvedChatID)
		if err != nil {
			logger.Fatalf("init telegram notifier: %v", err)
		}
		notifier = tg
		chatSender = tg
	}

	svc, err := watcher.New(watcher.Config{
		StatePath:        *statePath,
		MasterDBPath:     *dbPath,
		SeedMasterDBPath: *seedDBPath,
		Interval:         *interval,
		CheckOnStart:     true,
		Verbose:          *verbose,
	}, store, recv, notifier, watcher.WithLogger(logger))
	if err != nil {
		logger.Fatalf("init watcher: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		started := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		res, err := svc.RunOnce(checkCtx)
		if err != nil {
			logger.Fatalf("check failed (after %s): %v", time.Since(started).Round(time.Millisecond), err)
		}
		svc.LogCheckResult(res, time.Since(started).Round(time.Millisecond))
		return
	}

	var (
		dcStore    *discordbot.Store
		dcClient   *discordbot.Client
		dcVerifier discordbot.PermissionVerifier
		dcSender   discordbot.MessageSender
		dcSched    *discordbot.Scheduler
	)
	discordEnabled := whCfg.DiscordPublicKey != "" || (whCfg.DiscordAppID != "" && whCfg.DiscordBotToken != "")
	if discordEnabled {
		dcStore, err = discordbot.OpenStore(whCfg.DiscordDBPath)
		if err != nil {
			logger.Fatalf("init discord store %q: %v", whCfg.DiscordDBPath, err)
		}
		defer func() {
			if err := dcStore.Close(); err != nil {
				logger.Printf("close discord store %q: %v", whCfg.DiscordDBPath, err)
			}
		}()

		if whCfg.DiscordAppID != "" && whCfg.DiscordBotToken != "" {
			dcClient, err = discordbot.NewClient(whCfg.DiscordAppID, whCfg.DiscordBotToken)
			if err != nil {
				logger.Fatalf("init discord client: %v", err)
			}
			dcVerifier = dcClient
			if !*dryRun {
				syncCtx, cancelSync := context.WithTimeout(ctx, 20*time.Second)
				if err := dcClient.SyncCommands(syncCtx); err != nil {
					logger.Printf("warning: failed to sync discord slash commands: %v", err)
				} else {
					logger.Printf("synced discord global slash commands (/ping, /notify-channel)")
				}
				cancelSync()
			}
		}

		if *dryRun {
			dcSender = stdoutDiscordSender{}
		} else if dcClient != nil {
			dcSender = dcClient
		}
		if dcSender != nil {
			dcSched, err = discordbot.NewScheduler(discordbot.SchedulerConfig{
				MasterDBPath: *dbPath,
				Interval:     *interval,
				CheckOnStart: true,
			}, dcStore, dcSender, discordbot.WithLogger(logger))
			if err != nil {
				logger.Fatalf("init discord scheduler: %v", err)
			}
		}
	}

	webhookSrv, err := buildWebhookServer(whCfg, chatSender, dcStore, dcVerifier, *dbPath, logger)
	if err != nil {
		logger.Fatalf("init webhook server: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := svc.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("watcher exited with error: %w", err)
			cancelRun()
		}
	}()

	if webhookSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webhookSrv.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("webhook server exited with error: %w", err)
				cancelRun()
			}
		}()
	}

	if dcSched != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dcSched.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("discord scheduler exited with error: %w", err)
				cancelRun()
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			logger.Fatalf("%v", err)
		}
	}
	logger.Printf("bot shutdown complete")
}

func setupLogger(logFilePath string, console io.Writer) (*log.Logger, func() error, error) {
	if console == nil {
		console = os.Stderr
	}
	path := strings.TrimSpace(logFilePath)
	if path == "" {
		return log.New(console, "", log.LstdFlags), func() error { return nil }, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory %q: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %q: %w", path, err)
	}

	return log.New(io.MultiWriter(console, f), "", log.LstdFlags), f.Close, nil
}

func loadDotEnv(path string, required bool) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}

		key, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			return false, fmt.Errorf("line %d: missing '='", lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return false, fmt.Errorf("line %d: empty key", lineNo)
		}

		val, err := parseEnvValue(rawVal)
		if err != nil {
			return false, fmt.Errorf("line %d (%s): %w", lineNo, key, err)
		}

		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return false, fmt.Errorf("set env %q: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, nil
}

func parseEnvValue(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return unquoted, nil
	}
	if s[0] == '\'' {
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return "", errors.New("unterminated single-quoted value")
		}
		return s[1 : len(s)-1], nil
	}
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s, nil
}

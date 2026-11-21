// Command bot polls the Uma Musume API hourly for master.mdb updates and sends
// table and campaign_data diffs to a Telegram chat.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kudapan_lokal/internal/masterdb"
	"kudapan_lokal/internal/notify"
	"kudapan_lokal/internal/savedata"
	"kudapan_lokal/internal/umaclient"
	"kudapan_lokal/internal/watcher"
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

func main() {
	var (
		envPath      = flag.String("env", defaultEnvPath, "Path to .env file")
		token        = flag.String("token", "", "Telegram bot token (overrides TELEGRAM_BOT_TOKEN in .env/env)")
		chatID       = flag.String("chat-id", "", "Telegram chat ID (overrides TELEGRAM_CHAT_ID in .env/env)")
		savedataPath = flag.String("savedata", savedata.DefaultFilePath, "Path to savedata.json")
		statePath    = flag.String("state", watcher.DefaultStatePath, "Path to watcher state.json")
		dbPath       = flag.String("db", watcher.DefaultMasterDBPath, "Path to cached baseline master.mdb")
		seedDBPath   = flag.String("seed-db", "", "Optional path to seed initial baseline master.mdb (e.g. /mnt/bind/master.mdb)")
		interval     = flag.Duration("interval", watcher.DefaultInterval, "Check interval (e.g. 1h)")
		once         = flag.Bool("once", false, "Run a single check and exit")
		dryRun       = flag.Bool("dry-run", false, "Print formatted messages to stdout instead of sending to Telegram")
		logFilePath  = flag.String("log-file", defaultLogFilePath, "Path to append logs alongside console output (empty disables file logging)")
		verbose      = flag.Bool("verbose", false, "Enable verbose logs (manifest resolution steps, URLs, per-table diffs)")
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

	resolvedToken := *token
	if resolvedToken == "" {
		resolvedToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	resolvedChatID := *chatID
	if resolvedChatID == "" {
		resolvedChatID = os.Getenv("TELEGRAM_CHAT_ID")
	}

	logger.Printf(
		"starting kudapan_lokal bot (env=%q loaded=%v, savedata=%q, state=%q, db=%q, seed=%q, interval=%s, once=%v, dry_run=%v, verbose=%v, log_file=%q)",
		*envPath, envLoaded, *savedataPath, *statePath, *dbPath, *seedDBPath, *interval, *once, *dryRun, *verbose, *logFilePath,
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

	var notifier notify.Notifier
	if *dryRun {
		logger.Printf("dry-run mode enabled: formatted notifications will print to stdout instead of Telegram")
		notifier = stdoutNotifier{}
	} else {
		tg, err := notify.NewTelegramClient(resolvedToken, resolvedChatID)
		if err != nil {
			logger.Fatalf("init telegram notifier: %v", err)
		}
		notifier = tg
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

	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatalf("watcher exited with error: %v", err)
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

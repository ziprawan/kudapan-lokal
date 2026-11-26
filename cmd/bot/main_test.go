package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "\uFEFF# Telegram credentials\n" +
		"TELEGRAM_BOT_TOKEN=\"123456:ABC-xyz\"\n" +
		"export TELEGRAM_CHAT_ID='-100987654321'\n" +
		"EXISTING_KEY=from_file\n" +
		"INLINE_COMMENT=hello # trailing comment\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	_ = os.Unsetenv("TELEGRAM_BOT_TOKEN")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	_ = os.Unsetenv("TELEGRAM_CHAT_ID")
	t.Setenv("EXISTING_KEY", "already_set")
	t.Setenv("INLINE_COMMENT", "")
	_ = os.Unsetenv("INLINE_COMMENT")

	loaded, err := loadDotEnv(envPath, true)
	if err != nil || !loaded {
		t.Fatalf("loadDotEnv() = (%v, %v), want (true, nil)", loaded, err)
	}

	if got := os.Getenv("TELEGRAM_BOT_TOKEN"); got != "123456:ABC-xyz" {
		t.Errorf("TELEGRAM_BOT_TOKEN = %q, want 123456:ABC-xyz", got)
	}
	if got := os.Getenv("TELEGRAM_CHAT_ID"); got != "-100987654321" {
		t.Errorf("TELEGRAM_CHAT_ID = %q, want -100987654321", got)
	}
	if got := os.Getenv("EXISTING_KEY"); got != "already_set" {
		t.Errorf("EXISTING_KEY = %q, want already_set (OS env precedence)", got)
	}
	if got := os.Getenv("INLINE_COMMENT"); got != "hello" {
		t.Errorf("INLINE_COMMENT = %q, want hello", got)
	}
}

func TestLoadDotEnvMissingAndErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	loaded, err := loadDotEnv(missing, false)
	if err != nil || loaded {
		t.Fatalf("expected missing optional .env -> (false, nil), got (%v, %v)", loaded, err)
	}
	if _, err := loadDotEnv(missing, true); err == nil {
		t.Fatal("expected error for missing required .env")
	}

	bad := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(bad, []byte("NO_EQUALS_SIGN\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := loadDotEnv(bad, true); err == nil {
		t.Fatal("expected error on malformed line")
	}
}

func TestSetupLogger(t *testing.T) {
	var console bytes.Buffer
	logPath := filepath.Join(t.TempDir(), "logs", "bot.log")

	logger, closeLog, err := setupLogger(logPath, &console)
	if err != nil {
		t.Fatalf("setupLogger(with file) error = %v", err)
	}
	logger.Printf("hello %s", "bot")
	if err := closeLog(); err != nil {
		t.Fatalf("closeLog() error = %v", err)
	}

	if !strings.Contains(console.String(), "hello bot") {
		t.Errorf("console output missing message: %q", console.String())
	}
	fileBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPath, err)
	}
	if !strings.Contains(string(fileBytes), "hello bot") {
		t.Errorf("log file missing message: %q", string(fileBytes))
	}

	var consoleOnly bytes.Buffer
	logger2, closeLog2, err := setupLogger("", &consoleOnly)
	if err != nil {
		t.Fatalf("setupLogger(console only) error = %v", err)
	}
	logger2.Printf("console only")
	if err := closeLog2(); err != nil {
		t.Fatalf("closeLog2() error = %v", err)
	}
	if !strings.Contains(consoleOnly.String(), "console only") {
		t.Errorf("consoleOnly missing message: %q", consoleOnly.String())
	}
}

func TestWebhookEnvAndServerWiring(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	pubHex := hex.EncodeToString(pub)

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "WEBHOOK_LISTEN_ADDR=:9090\n" +
		"TELEGRAM_WEBHOOK_SECRET=secret-tg\n" +
		"DISCORD_PUBLIC_KEY=" + pubHex + "\n" +
		"DISCORD_APP_ID=123456789\n" +
		"DISCORD_BOT_TOKEN=bot-token\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	for _, k := range []string{
		"WEBHOOK_LISTEN_ADDR",
		"TELEGRAM_WEBHOOK_SECRET",
		"DISCORD_PUBLIC_KEY",
		"DISCORD_APP_ID",
		"DISCORD_BOT_TOKEN",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	loaded, err := loadDotEnv(envPath, true)
	if err != nil || !loaded {
		t.Fatalf("loadDotEnv() = (%v, %v), want (true, nil)", loaded, err)
	}

	cfg := webhookConfig{
		ListenAddr:            resolveFlagOrEnv("", "WEBHOOK_LISTEN_ADDR"),
		TelegramWebhookSecret: resolveFlagOrEnv("flag-secret", "TELEGRAM_WEBHOOK_SECRET"),
		DiscordPublicKey:      resolveFlagOrEnv("", "DISCORD_PUBLIC_KEY"),
		DiscordAppID:          resolveFlagOrEnv("", "DISCORD_APP_ID"),
		DiscordBotToken:       resolveFlagOrEnv("", "DISCORD_BOT_TOKEN"),
	}
	if cfg.ListenAddr != ":9090" || cfg.TelegramWebhookSecret != "flag-secret" || cfg.DiscordPublicKey != pubHex {
		t.Fatalf("unexpected resolved webhookConfig: %+v", cfg)
	}

	// Empty ListenAddr disables server creation cleanly.
	disabledSrv, err := buildWebhookServer(webhookConfig{}, stdoutNotifier{}, log.New(io.Discard, "", 0))
	if err != nil || disabledSrv != nil {
		t.Fatalf("expected (nil, nil) for disabled webhook server, got (%v, %v)", disabledSrv, err)
	}

	// Invalid Discord public key fails fast.
	if _, err := buildWebhookServer(webhookConfig{
		ListenAddr:       "127.0.0.1:0",
		DiscordPublicKey: "invalid-hex",
	}, stdoutNotifier{}, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("expected error for invalid DISCORD_PUBLIC_KEY")
	}

	// Valid wiring with empty DISCORD_PUBLIC_KEY serves /webhook/telegram and returns 503 on /webhook/discord.
	noDiscordSrv, err := buildWebhookServer(webhookConfig{
		ListenAddr:            "127.0.0.1:0",
		TelegramWebhookSecret: cfg.TelegramWebhookSecret,
	}, stdoutNotifier{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("buildWebhookServer(no discord) error = %v", err)
	}
	rec503 := httptest.NewRecorder()
	noDiscordSrv.Handler().ServeHTTP(rec503, httptest.NewRequest(http.MethodPost, "/webhook/discord", strings.NewReader("{}")))
	if rec503.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when DISCORD_PUBLIC_KEY omitted, got %d", rec503.Code)
	}

	// Full wiring serves both /webhook/telegram and /webhook/discord.
	fullSrv, err := buildWebhookServer(cfg, stdoutNotifier{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("buildWebhookServer(full) error = %v", err)
	}
	tgReq := httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(`{"update_id":1}`))
	tgReq.Header.Set("X-Telegram-Bot-Api-Secret-Token", "flag-secret")
	tgRec := httptest.NewRecorder()
	fullSrv.Handler().ServeHTTP(tgRec, tgReq)
	if tgRec.Code != http.StatusOK {
		t.Fatalf("POST /webhook/telegram status = %d, want 200", tgRec.Code)
	}

	body := []byte(`{"type":1}`)
	ts := "1700000000"
	sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), body...)))
	dcReq := httptest.NewRequest(http.MethodPost, "/webhook/discord", bytes.NewReader(body))
	dcReq.Header.Set("X-Signature-Ed25519", sig)
	dcReq.Header.Set("X-Signature-Timestamp", ts)
	dcRec := httptest.NewRecorder()
	fullSrv.Handler().ServeHTTP(dcRec, dcReq)
	if dcRec.Code != http.StatusOK || !strings.Contains(dcRec.Body.String(), `"type":1`) {
		t.Fatalf("POST /webhook/discord status=%d body=%s", dcRec.Code, dcRec.Body.String())
	}
}

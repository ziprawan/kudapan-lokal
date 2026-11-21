package main

import (
	"bytes"
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

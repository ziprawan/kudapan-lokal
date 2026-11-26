package webhook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeChatSender struct {
	calls []struct {
		chatID string
		text   string
	}
	err error
}

func (f *fakeChatSender) SendMessageToChat(_ context.Context, chatID, text string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		chatID string
		text   string
	}{chatID: chatID, text: text})
	return nil
}

func generateTestEd25519(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	return pub, priv
}

func signDiscordRequest(priv ed25519.PrivateKey, timestamp string, body []byte) string {
	msg := append([]byte(timestamp), body...)
	return hex.EncodeToString(ed25519.Sign(priv, msg))
}

func TestDiscordKeyAndSignatureVerification(t *testing.T) {
	pub, priv := generateTestEd25519(t)
	hexKey := hex.EncodeToString(pub)

	parsed, err := ParseDiscordPublicKey("  " + hexKey + "  ")
	if err != nil {
		t.Fatalf("ParseDiscordPublicKey() error = %v", err)
	}
	if !bytes.Equal(parsed, pub) {
		t.Fatalf("parsed key mismatch")
	}

	for _, bad := range []string{"", "not-hex", "abcd"} {
		if _, err := ParseDiscordPublicKey(bad); err == nil {
			t.Errorf("ParseDiscordPublicKey(%q) expected error, got nil", bad)
		}
	}

	body := []byte(`{"type":1}`)
	ts := "1700000000"
	sig := signDiscordRequest(priv, ts, body)

	if !VerifyDiscordSignature(pub, sig, ts, body) {
		t.Fatal("expected valid signature to pass")
	}
	if VerifyDiscordSignature(pub, sig, ts, []byte(`{"type":2}`)) {
		t.Error("expected tampered body to fail verification")
	}
	if VerifyDiscordSignature(pub, sig, "1700000001", body) {
		t.Error("expected wrong timestamp to fail verification")
	}
	if VerifyDiscordSignature(pub, "zzzz", ts, body) {
		t.Error("expected malformed hex signature to fail verification")
	}
	if VerifyDiscordSignature(pub, "", ts, body) {
		t.Error("expected empty signature to fail verification")
	}
}

func TestDiscordHandlerPingCommandsAndUnauthorized(t *testing.T) {
	pub, priv := generateTestEd25519(t)
	otherPub, otherPriv := generateTestEd25519(t)
	_ = otherPub

	h, err := NewDiscordHandler(DiscordConfig{
		PublicKeyHex: hex.EncodeToString(pub),
		Logger:       log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewDiscordHandler() error = %v", err)
	}
	h.RegisterCommand("hello", func(_ context.Context, ix DiscordInteraction) DiscordInteractionResponse {
		return DiscordInteractionResponse{
			Type: DiscordResponseChannelMessageWithSource,
			Data: &DiscordInteractionResponseData{Content: "Hello from " + ix.ID},
		}
	})

	srv, err := NewServer(Config{
		ListenAddr:     "127.0.0.1:0",
		DiscordHandler: h,
		Logger:         log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	doDiscord := func(body []byte, sig, ts string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/discord", bytes.NewReader(body))
		if sig != "" {
			req.Header.Set("X-Signature-Ed25519", sig)
		}
		if ts != "" {
			req.Header.Set("X-Signature-Timestamp", ts)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	ts := "1700000123"
	pingBody := []byte(`{"id":"1","application_id":"app","type":1}`)

	// Missing headers -> 401
	if rec := doDiscord(pingBody, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature status = %d, want 401", rec.Code)
	}
	// Wrong key signature -> 401
	wrongSig := signDiscordRequest(otherPriv, ts, pingBody)
	if rec := doDiscord(pingBody, wrongSig, ts); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key signature status = %d, want 401", rec.Code)
	}

	// Valid PING -> 200 {"type":1}
	rec := doDiscord(pingBody, signDiscordRequest(priv, ts, pingBody), ts)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid PING status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pongResp DiscordInteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pongResp); err != nil {
		t.Fatalf("unmarshal PING response: %v", err)
	}
	if pongResp.Type != DiscordResponsePong {
		t.Fatalf("PING response type = %d, want %d", pongResp.Type, DiscordResponsePong)
	}

	// Built-in /ping command
	cmdBody := []byte(`{"id":"2","application_id":"app","type":2,"data":{"id":"cmd1","name":"ping","type":1}}`)
	rec = doDiscord(cmdBody, signDiscordRequest(priv, ts, cmdBody), ts)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ping status = %d, want 200", rec.Code)
	}
	var cmdResp DiscordInteractionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cmdResp); err != nil {
		t.Fatalf("unmarshal /ping response: %v", err)
	}
	if cmdResp.Type != DiscordResponseChannelMessageWithSource || cmdResp.Data == nil || cmdResp.Data.Content != "Pong!" {
		t.Fatalf("unexpected /ping response: %+v", cmdResp)
	}

	// Custom registered command
	helloBody := []byte(`{"id":"99","application_id":"app","type":2,"data":{"id":"cmd2","name":"hello","type":1}}`)
	rec = doDiscord(helloBody, signDiscordRequest(priv, ts, helloBody), ts)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hello from 99") {
		t.Fatalf("unexpected /hello response (%d): %s", rec.Code, rec.Body.String())
	}

	// Unknown command -> graceful boilerplate reply
	unknownBody := []byte(`{"id":"3","application_id":"app","type":2,"data":{"id":"cmd3","name":"unknown","type":1}}`)
	rec = doDiscord(unknownBody, signDiscordRequest(priv, ts, unknownBody), ts)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Command not implemented yet.") {
		t.Fatalf("unexpected unknown command response (%d): %s", rec.Code, rec.Body.String())
	}
}

func TestTelegramHandlerSecretAndCommands(t *testing.T) {
	sender := &fakeChatSender{}
	h := NewTelegramHandler(TelegramConfig{
		SecretToken: "tg-secret-123",
		Sender:      sender,
		Logger:      log.New(io.Discard, "", 0),
	})
	srv, err := NewServer(Config{
		ListenAddr:      "127.0.0.1:0",
		TelegramHandler: h,
		Logger:          log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	payload := []byte(`{"update_id":42,"message":{"message_id":7,"chat":{"id":-10012345,"type":"supergroup"},"date":1700000000,"text":"/ping@mybot"}}`)

	// Missing secret token -> 401
	reqNoSecret := httptest.NewRequest(http.MethodPost, "/api/webhook/telegram", bytes.NewReader(payload))
	recNoSecret := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNoSecret, reqNoSecret)
	if recNoSecret.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret status = %d, want 401", recNoSecret.Code)
	}

	// Wrong secret token -> 401
	reqBadSecret := httptest.NewRequest(http.MethodPost, "/api/webhook/telegram", bytes.NewReader(payload))
	reqBadSecret.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	recBadSecret := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadSecret, reqBadSecret)
	if recBadSecret.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want 401", recBadSecret.Code)
	}

	// Valid secret token + /ping@mybot -> 200 and reply sent
	reqOK := httptest.NewRequest(http.MethodPost, "/api/webhook/telegram", bytes.NewReader(payload))
	reqOK.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret-123")
	recOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid telegram webhook status = %d, want 200: %s", recOK.Code, recOK.Body.String())
	}
	if len(sender.calls) != 1 || sender.calls[0].chatID != "-10012345" || sender.calls[0].text != "Pong!" {
		t.Fatalf("unexpected sender calls: %+v", sender.calls)
	}

	// /pingpong must NOT trigger /ping
	nonPingPayload := []byte(`{"update_id":43,"message":{"message_id":8,"chat":{"id":123,"type":"private"},"date":1700000001,"text":"/pingpong"}}`)
	reqOther := httptest.NewRequest(http.MethodPost, "/api/webhook/telegram", bytes.NewReader(nonPingPayload))
	reqOther.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret-123")
	recOther := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOther, reqOther)
	if recOther.Code != http.StatusOK || len(sender.calls) != 1 {
		t.Fatalf("expected /pingpong not to send /ping reply, code=%d calls=%+v", recOther.Code, sender.calls)
	}

	// Unconfigured SecretToken allows requests without header and calls OnUpdate
	var gotID int64
	hOpen := NewTelegramHandler(TelegramConfig{
		Logger: log.New(io.Discard, "", 0),
		OnUpdate: func(_ context.Context, u TelegramUpdate) error {
			gotID = u.UpdateID
			return nil
		},
	})
	recOpen := httptest.NewRecorder()
	hOpen.ServeHTTP(recOpen, httptest.NewRequest(http.MethodPost, "/webhook/telegram", strings.NewReader(`{"update_id":99}`)))
	if recOpen.Code != http.StatusOK || gotID != 99 {
		t.Fatalf("open telegram handler code=%d gotID=%d", recOpen.Code, gotID)
	}
}

func TestServerRoutesHealthAndGracefulShutdown(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Logger:     log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// Unconfigured handlers return 503 Service Unavailable
	for _, path := range []string{"/api/webhook/telegram", "/api/webhook/discord"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s status = %d, want 503", path, rec.Code)
		}
	}

	// Wrong HTTP method returns 405 Method Not Allowed
	recMethod := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMethod, httptest.NewRequest(http.MethodGet, "/api/webhook/discord", nil))
	if recMethod.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/webhook/discord status = %d, want 405", recMethod.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- srv.Run(ctx)
	}()

	var addrStr string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != nil {
			addrStr = a.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addrStr == "" {
		t.Fatal("webhook server did not bind an address within timeout")
	}

	resp, err := http.Get("http://" + addrStr + "/api/healthz")
	if err != nil {
		t.Fatalf("GET /api/healthz error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/healthz status = %d, want 200", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), `"status":"ok"`) {
		t.Errorf("unexpected /api/healthz body: %s", string(bodyBytes))
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Server.Run() returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run() did not shut down in time")
	}
}

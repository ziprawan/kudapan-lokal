package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// ChatSender sends a Telegram text message to an arbitrary chat ID.
type ChatSender interface {
	SendMessageToChat(ctx context.Context, chatID, text string) error
}

// TelegramUpdate represents an incoming Telegram Bot API webhook update.
type TelegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *TelegramMessage `json:"message,omitempty"`
}

// TelegramMessage represents a Telegram message within an update.
type TelegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *TelegramUser `json:"from,omitempty"`
	Chat      TelegramChat  `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text,omitempty"`
}

// TelegramUser represents the sender of a Telegram message.
type TelegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// TelegramChat represents the Telegram chat where a message originated.
type TelegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

// TelegramUpdateFunc handles a decoded Telegram webhook update.
type TelegramUpdateFunc func(ctx context.Context, update TelegramUpdate) error

// TelegramConfig configures a TelegramHandler.
type TelegramConfig struct {
	SecretToken string
	Sender      ChatSender
	OnUpdate    TelegramUpdateFunc
	Logger      *log.Logger
}

// TelegramHandler handles POST /api/webhook/telegram requests from Telegram.
type TelegramHandler struct {
	secretToken string
	sender      ChatSender
	onUpdate    TelegramUpdateFunc
	logger      *log.Logger
}

// NewTelegramHandler creates a Telegram webhook handler with optional secret
// token verification and update callback.
func NewTelegramHandler(cfg TelegramConfig) *TelegramHandler {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &TelegramHandler{
		secretToken: strings.TrimSpace(cfg.SecretToken),
		sender:      cfg.Sender,
		onUpdate:    cfg.OnUpdate,
		logger:      logger,
	}
}

// ServeHTTP verifies Telegram's optional secret token header, decodes the
// update payload, and dispatches it to OnUpdate or the built-in boilerplate.
func (h *TelegramHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.secretToken != "" {
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.secretToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := readLimitedBody(r, maxWebhookBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var update TelegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		http.Error(w, "invalid telegram update payload", http.StatusBadRequest)
		return
	}

	if h.onUpdate != nil {
		if err := h.onUpdate(r.Context(), update); err != nil {
			h.logger.Printf("[webhook/telegram] handler error (update_id=%d): %v", update.UpdateID, err)
			http.Error(w, "failed to process update", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if err := h.handleDefault(r.Context(), update); err != nil {
		h.logger.Printf("[webhook/telegram] default handler error (update_id=%d): %v", update.UpdateID, err)
		http.Error(w, "failed to process update", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *TelegramHandler) handleDefault(ctx context.Context, update TelegramUpdate) error {
	if update.Message == nil {
		h.logger.Printf("[webhook/telegram] received update_id=%d (no message)", update.UpdateID)
		return nil
	}

	h.logger.Printf("[webhook/telegram] received update_id=%d chat_id=%d text=%q",
		update.UpdateID, update.Message.Chat.ID, update.Message.Text)

	cmd := parseTelegramCommand(update.Message.Text)
	if cmd == "/ping" && h.sender != nil {
		chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
		return h.sender.SendMessageToChat(ctx, chatID, "Pong!")
	}
	return nil
}

func parseTelegramCommand(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	cmd, _, _ := strings.Cut(trimmed, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return cmd
}

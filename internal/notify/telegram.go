// Package notify formats master.mdb update reports and delivers messages via Telegram.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultTelegramAPIBase is the Telegram Bot API base URL.
	DefaultTelegramAPIBase = "https://api.telegram.org"
	// MaxTelegramMessageRunes is Telegram's per-message character limit.
	MaxTelegramMessageRunes = 4096
)

// Notifier delivers formatted notification messages.
type Notifier interface {
	SendMessage(ctx context.Context, text string) error
}

// TelegramClient sends HTML-formatted notifications to a Telegram chat.
type TelegramClient struct {
	baseURL    string
	token      string
	chatID     string
	httpClient *http.Client
}

// Option configures a TelegramClient.
type Option func(*TelegramClient)

// WithBaseURL overrides the Telegram API base URL (primarily for testing).
func WithBaseURL(baseURL string) Option {
	return func(c *TelegramClient) {
		if baseURL != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithHTTPClient overrides the *http.Client used for Telegram requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *TelegramClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// NewTelegramClient constructs a Telegram Bot API sender targeting chatID.
func NewTelegramClient(token, chatID string, opts ...Option) (*TelegramClient, error) {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" {
		return nil, errors.New("telegram bot token cannot be empty")
	}
	if chatID == "" {
		return nil, errors.New("telegram chat id cannot be empty")
	}

	c := &TelegramClient{
		baseURL:    DefaultTelegramAPIBase,
		token:      token,
		chatID:     chatID,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type telegramAPIResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// SendMessage delivers text to the configured Telegram chat, splitting on
// newline boundaries when text exceeds Telegram's 4096-character limit.
func (c *TelegramClient) SendMessage(ctx context.Context, text string) error {
	chunks := SplitMessage(text, MaxTelegramMessageRunes)
	for i, chunk := range chunks {
		if err := c.sendChunk(ctx, chunk); err != nil {
			return fmt.Errorf("send telegram message chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

func (c *TelegramClient) sendChunk(ctx context.Context, text string) error {
	payload, err := json.Marshal(sendMessageRequest{
		ChatID:                c.chatID,
		Text:                  text,
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", c.baseURL, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute telegram request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read telegram response: %w", err)
	}

	var apiResp telegramAPIResponse
	if err := json.Unmarshal(body, &apiResp); err == nil && !apiResp.OK {
		return fmt.Errorf("telegram API error (%d, HTTP %d): %s", apiResp.ErrorCode, resp.StatusCode, apiResp.Description)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SplitMessage breaks text into chunks of at most maxRunes runes, preferring
// newline boundaries so HTML tags kept on single lines stay balanced.
func SplitMessage(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = MaxTelegramMessageRunes
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}

	lines := strings.Split(text, "\n")
	var chunks []string
	var cur strings.Builder
	curRunes := 0

	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curRunes = 0
		}
	}

	for _, line := range lines {
		lineRunes := utf8.RuneCountInString(line)
		if lineRunes > maxRunes {
			flush()
			runes := []rune(line)
			for len(runes) > 0 {
				take := maxRunes
				if len(runes) < take {
					take = len(runes)
				}
				chunks = append(chunks, string(runes[:take]))
				runes = runes[take:]
			}
			continue
		}

		addRunes := lineRunes
		if cur.Len() > 0 {
			addRunes++ // newline separator
		}
		if curRunes+addRunes > maxRunes {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
			curRunes++
		}
		cur.WriteString(line)
		curRunes += lineRunes
	}
	flush()
	return chunks
}

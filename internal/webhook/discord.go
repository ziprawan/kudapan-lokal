package webhook

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

// Discord HTTP interaction types (Discord API v10).
const (
	DiscordInteractionPing               = 1
	DiscordInteractionApplicationCommand = 2
)

// Discord HTTP interaction response types (Discord API v10).
const (
	DiscordResponsePong                     = 1
	DiscordResponseChannelMessageWithSource = 4
)

// DiscordInteraction represents a subset of a Discord HTTP interaction payload.
type DiscordInteraction struct {
	ID             string                    `json:"id"`
	ApplicationID  string                    `json:"application_id"`
	Type           int                       `json:"type"`
	GuildID        string                    `json:"guild_id,omitempty"`
	ChannelID      string                    `json:"channel_id,omitempty"`
	AppPermissions string                    `json:"app_permissions,omitempty"`
	Member         *DiscordInteractionMember `json:"member,omitempty"`
	Token          string                    `json:"token,omitempty"`
	Version        int                       `json:"version,omitempty"`
	Data           *DiscordInteractionData   `json:"data,omitempty"`
}

// DiscordInteractionMember represents the invoking guild member metadata.
type DiscordInteractionMember struct {
	Permissions string   `json:"permissions,omitempty"`
	Roles       []string `json:"roles,omitempty"`
}

// DiscordInteractionData contains invoked application command metadata.
type DiscordInteractionData struct {
	ID       string                      `json:"id"`
	Name     string                      `json:"name"`
	Type     int                         `json:"type,omitempty"`
	Options  []DiscordInteractionOption  `json:"options,omitempty"`
	Resolved *DiscordInteractionResolved `json:"resolved,omitempty"`
}

// DiscordInteractionOption represents a slash command option passed by Discord.
type DiscordInteractionOption struct {
	Name  string `json:"name"`
	Type  int    `json:"type"`
	Value any    `json:"value,omitempty"`
}

// DiscordInteractionResolved contains resolved Discord entities referenced by options.
type DiscordInteractionResolved struct {
	Roles    map[string]DiscordResolvedRole    `json:"roles,omitempty"`
	Channels map[string]DiscordResolvedChannel `json:"channels,omitempty"`
}

// DiscordResolvedRole represents a resolved Discord role in an interaction payload.
type DiscordResolvedRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Permissions string `json:"permissions,omitempty"`
	Mentionable bool   `json:"mentionable"`
}

// DiscordResolvedChannel represents a resolved Discord channel in an interaction payload.
type DiscordResolvedChannel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        int    `json:"type"`
	Permissions string `json:"permissions,omitempty"`
}

// StringOption looks up a string-valued slash command option (including
// string, channel, role, and user IDs) by name.
func (d *DiscordInteractionData) StringOption(name string) (string, bool) {
	if d == nil {
		return "", false
	}
	for _, opt := range d.Options {
		if opt.Name != name {
			continue
		}
		if s, ok := opt.Value.(string); ok {
			return s, true
		}
	}
	return "", false
}

// DiscordInteractionResponse is returned synchronously by an HTTP interactions endpoint.
type DiscordInteractionResponse struct {
	Type int                             `json:"type"`
	Data *DiscordInteractionResponseData `json:"data,omitempty"`
}

// DiscordInteractionResponseData holds message fields for an interaction response.
type DiscordInteractionResponseData struct {
	Content string `json:"content,omitempty"`
	Flags   int    `json:"flags,omitempty"`
}

// DiscordCommandFunc processes a Discord application command interaction.
type DiscordCommandFunc func(ctx context.Context, ix DiscordInteraction) DiscordInteractionResponse

// DiscordConfig configures a DiscordHandler.
type DiscordConfig struct {
	PublicKeyHex string
	PublicKey    ed25519.PublicKey
	Commands     map[string]DiscordCommandFunc
	Logger       *log.Logger
}

// DiscordHandler handles POST /api/webhook/discord HTTP interaction webhooks.
type DiscordHandler struct {
	pubKey ed25519.PublicKey
	logger *log.Logger

	mu       sync.RWMutex
	commands map[string]DiscordCommandFunc
}

// ParseDiscordPublicKey decodes a 64-character hex Discord application public key.
func ParseDiscordPublicKey(hexKey string) (ed25519.PublicKey, error) {
	trimmed := strings.TrimSpace(hexKey)
	if trimmed == "" {
		return nil, errors.New("discord public key cannot be empty")
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode discord public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("discord public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyDiscordSignature validates Discord's X-Signature-Ed25519 and
// X-Signature-Timestamp headers over the raw request body.
func VerifyDiscordSignature(pubKey ed25519.PublicKey, signatureHex, timestamp string, body []byte) bool {
	if len(pubKey) != ed25519.PublicKeySize || signatureHex == "" || timestamp == "" {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := make([]byte, 0, len(timestamp)+len(body))
	msg = append(msg, timestamp...)
	msg = append(msg, body...)
	return ed25519.Verify(pubKey, msg, sig)
}

// NewDiscordHandler constructs a Discord HTTP Interactions webhook handler with
// Ed25519 verification, automatic PING/PONG, and a built-in /ping command boilerplate.
func NewDiscordHandler(cfg DiscordConfig) (*DiscordHandler, error) {
	var pubKey ed25519.PublicKey
	if len(cfg.PublicKey) > 0 {
		if len(cfg.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("discord public key must be %d bytes, got %d", ed25519.PublicKeySize, len(cfg.PublicKey))
		}
		pubKey = append(ed25519.PublicKey(nil), cfg.PublicKey...)
	} else {
		parsed, err := ParseDiscordPublicKey(cfg.PublicKeyHex)
		if err != nil {
			return nil, err
		}
		pubKey = parsed
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	commands := map[string]DiscordCommandFunc{
		"ping": func(context.Context, DiscordInteraction) DiscordInteractionResponse {
			return DiscordInteractionResponse{
				Type: DiscordResponseChannelMessageWithSource,
				Data: &DiscordInteractionResponseData{Content: "Pong!"},
			}
		},
	}
	for name, fn := range cfg.Commands {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" && fn != nil {
			commands[trimmed] = fn
		}
	}

	return &DiscordHandler{
		pubKey:   pubKey,
		logger:   logger,
		commands: commands,
	}, nil
}

// RegisterCommand registers or overrides a Discord slash command handler.
func (h *DiscordHandler) RegisterCommand(name string, fn DiscordCommandFunc) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || fn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands[trimmed] = fn
}

// ServeHTTP verifies Discord's Ed25519 signature, handles PING verification,
// and dispatches slash command interactions.
func (h *DiscordHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
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

	sigHex := r.Header.Get("X-Signature-Ed25519")
	timestamp := r.Header.Get("X-Signature-Timestamp")
	if !VerifyDiscordSignature(h.pubKey, sigHex, timestamp, body) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var ix DiscordInteraction
	if err := json.Unmarshal(body, &ix); err != nil {
		http.Error(w, "invalid discord interaction payload", http.StatusBadRequest)
		return
	}

	switch ix.Type {
	case DiscordInteractionPing:
		h.logger.Printf("[webhook/discord] received PING interaction (id=%s)", ix.ID)
		writeJSON(w, http.StatusOK, DiscordInteractionResponse{Type: DiscordResponsePong})
	case DiscordInteractionApplicationCommand:
		if ix.Data == nil {
			http.Error(w, "missing interaction command data", http.StatusBadRequest)
			return
		}
		h.logger.Printf("[webhook/discord] received command %q (id=%s)", ix.Data.Name, ix.ID)
		h.mu.RLock()
		fn := h.commands[ix.Data.Name]
		h.mu.RUnlock()
		if fn == nil {
			writeJSON(w, http.StatusOK, DiscordInteractionResponse{
				Type: DiscordResponseChannelMessageWithSource,
				Data: &DiscordInteractionResponseData{Content: "Command not implemented yet."},
			})
			return
		}
		writeJSON(w, http.StatusOK, fn(r.Context(), ix))
	default:
		http.Error(w, "unsupported interaction type", http.StatusBadRequest)
	}
}

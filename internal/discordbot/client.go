package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kudapan_lokal/internal/webhook"
)

const (
	// DefaultDiscordAPIBase is the Discord REST API v10 base URL.
	DefaultDiscordAPIBase = "https://discord.com/api/v10"

	// Discord permission bitflags.
	PermAdministrator   uint64 = 1 << 3
	PermViewChannel     uint64 = 1 << 10
	PermSendMessages    uint64 = 1 << 11
	PermMentionEveryone uint64 = 1 << 17
)

// ParsePermissionBits parses Discord's decimal permission bitfield string.
func ParsePermissionBits(s string) (uint64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, nil
	}
	return strconv.ParseUint(trimmed, 10, 64)
}

// HasAdminPermission reports whether Discord's decimal permission string grants Administrator.
func HasAdminPermission(perms string) bool {
	bits, err := ParsePermissionBits(perms)
	return err == nil && (bits&PermAdministrator) != 0
}

// DiscordRole represents a guild role returned by Discord's REST API.
type DiscordRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Permissions string `json:"permissions"`
	Mentionable bool   `json:"mentionable"`
}

// DiscordPermissionOverwrite represents a channel permission overwrite.
type DiscordPermissionOverwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"` // 0 = role, 1 = member
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

type discordChannel struct {
	ID                   string                       `json:"id"`
	GuildID              string                       `json:"guild_id"`
	PermissionOverwrites []DiscordPermissionOverwrite `json:"permission_overwrites"`
}

type discordMember struct {
	Roles []string `json:"roles"`
}

// ComputeChannelPermissions calculates a member's effective permissions in a
// Discord channel using guild roles, member role IDs, and channel overwrites.
func ComputeChannelPermissions(
	guildID, memberUserID string,
	guildRoles []DiscordRole,
	memberRoleIDs []string,
	overwrites []DiscordPermissionOverwrite,
) uint64 {
	roleMap := make(map[string]DiscordRole, len(guildRoles))
	for _, r := range guildRoles {
		roleMap[r.ID] = r
	}

	var base uint64
	if everyone, ok := roleMap[guildID]; ok {
		base, _ = ParsePermissionBits(everyone.Permissions)
	}
	memberRoleSet := make(map[string]struct{}, len(memberRoleIDs))
	for _, rid := range memberRoleIDs {
		memberRoleSet[rid] = struct{}{}
		if r, ok := roleMap[rid]; ok {
			p, _ := ParsePermissionBits(r.Permissions)
			base |= p
		}
	}

	if (base & PermAdministrator) != 0 {
		return ^uint64(0)
	}

	perms := base

	// 1. Apply @everyone channel overwrite.
	for _, ow := range overwrites {
		if ow.ID == guildID {
			allow, _ := ParsePermissionBits(ow.Allow)
			deny, _ := ParsePermissionBits(ow.Deny)
			perms = (perms &^ deny) | allow
		}
	}

	// 2. Apply role-specific overwrites.
	var roleAllow, roleDeny uint64
	for _, ow := range overwrites {
		if ow.Type == 0 && ow.ID != guildID {
			if _, ok := memberRoleSet[ow.ID]; ok {
				allow, _ := ParsePermissionBits(ow.Allow)
				deny, _ := ParsePermissionBits(ow.Deny)
				roleAllow |= allow
				roleDeny |= deny
			}
		}
	}
	perms = (perms &^ roleDeny) | roleAllow

	// 3. Apply member-specific overwrite.
	for _, ow := range overwrites {
		if ow.Type == 1 && ow.ID == memberUserID {
			allow, _ := ParsePermissionBits(ow.Allow)
			deny, _ := ParsePermissionBits(ow.Deny)
			perms = (perms &^ deny) | allow
		}
	}

	return perms
}

// Client interacts with Discord's REST API v10 for slash command registration,
// channel/role permission verification, and sending channel messages.
type Client struct {
	baseURL    string
	appID      string
	botToken   string
	httpClient *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithBaseURL overrides the Discord API base URL (for testing).
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithHTTPClient overrides the *http.Client used for Discord requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// NewClient constructs a Discord REST API client.
func NewClient(appID, botToken string, opts ...Option) (*Client, error) {
	appID = strings.TrimSpace(appID)
	botToken = strings.TrimSpace(botToken)
	if appID == "" {
		return nil, errors.New("discord app id cannot be empty")
	}
	if botToken == "" {
		return nil, errors.New("discord bot token cannot be empty")
	}

	c := &Client{
		baseURL:    DefaultDiscordAPIBase,
		appID:      appID,
		botToken:   botToken,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

type applicationCommandOption struct {
	Type         int    `json:"type"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Required     bool   `json:"required,omitempty"`
	ChannelTypes []int  `json:"channel_types,omitempty"`
}

type applicationCommand struct {
	Name                     string                     `json:"name"`
	Description              string                     `json:"description"`
	DefaultMemberPermissions *string                    `json:"default_member_permissions,omitempty"`
	DMPermission             *bool                      `json:"dm_permission,omitempty"`
	Options                  []applicationCommandOption `json:"options,omitempty"`
}

// SyncCommands overwrites global Discord slash commands with /ping and
// Administrator-only /notify-event.
func (c *Client) SyncCommands(ctx context.Context) error {
	adminPerm := strconv.FormatUint(PermAdministrator, 10)
	dmFalse := false
	cmds := []applicationCommand{
		{
			Name:        "ping",
			Description: "Check if the bot is responsive",
		},
		{
			Name:                     "notify-event",
			Description:              "Set the channel (and optional role) for event notifications",
			DefaultMemberPermissions: &adminPerm,
			DMPermission:             &dmFalse,
			Options: []applicationCommandOption{
				{
					Type:         7, // CHANNEL
					Name:         "channel",
					Description:  "Target text or announcement channel for notifications",
					Required:     true,
					ChannelTypes: []int{0, 5}, // GUILD_TEXT, GUILD_ANNOUNCEMENT
				},
				{
					Type:        8, // ROLE
					Name:        "role",
					Description: "Optional role to mention on notifications",
					Required:    false,
				},
			},
		},
	}

	endpoint := fmt.Sprintf("%s/applications/%s/commands", c.baseURL, c.appID)
	return c.doJSON(ctx, http.MethodPut, endpoint, cmds, nil)
}

// VerifyNotifyPermissions verifies that the bot can view and send messages in
// channelID and, when roleID is non-empty, can mention roleID.
func (c *Client) VerifyNotifyPermissions(
	ctx context.Context,
	guildID, channelID, roleID string,
	ix webhook.DiscordInteraction,
) error {
	var (
		perms      uint64
		guildRoles []DiscordRole
		haveRoles  bool
	)

	if channelID == ix.ChannelID && strings.TrimSpace(ix.AppPermissions) != "" {
		parsed, err := ParsePermissionBits(ix.AppPermissions)
		if err != nil {
			return fmt.Errorf("invalid interaction app_permissions: %w", err)
		}
		perms = parsed
	} else {
		var ch discordChannel
		if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/channels/%s", c.baseURL, channelID), nil, &ch); err != nil {
			return fmt.Errorf("fetch channel <#%s> permissions: %w", channelID, err)
		}
		if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/guilds/%s/roles", c.baseURL, guildID), nil, &guildRoles); err != nil {
			return fmt.Errorf("fetch guild roles: %w", err)
		}
		haveRoles = true

		var member discordMember
		if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/guilds/%s/members/%s", c.baseURL, guildID, c.appID), nil, &member); err != nil {
			return fmt.Errorf("fetch bot guild member: %w", err)
		}
		perms = ComputeChannelPermissions(guildID, c.appID, guildRoles, member.Roles, ch.PermissionOverwrites)
	}

	if (perms&PermAdministrator) == 0 && ((perms&PermViewChannel) == 0 || (perms&PermSendMessages) == 0) {
		return fmt.Errorf("bot lacks View Channel and/or Send Messages permission in <#%s>", channelID)
	}

	if roleID == "" {
		return nil
	}
	if (perms&PermAdministrator) != 0 || (perms&PermMentionEveryone) != 0 {
		return nil
	}

	if ix.Data != nil && ix.Data.Resolved != nil {
		if r, ok := ix.Data.Resolved.Roles[roleID]; ok {
			if r.Mentionable {
				return nil
			}
			return fmt.Errorf("bot cannot mention <@&%s>: role is not mentionable and bot lacks Mention @everyone, @here, and All Roles permission in <#%s>", roleID, channelID)
		}
	}

	if !haveRoles {
		if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/guilds/%s/roles", c.baseURL, guildID), nil, &guildRoles); err != nil {
			return fmt.Errorf("fetch guild roles to verify <@&%s>: %w", roleID, err)
		}
	}
	for _, r := range guildRoles {
		if r.ID == roleID {
			if r.Mentionable {
				return nil
			}
			return fmt.Errorf("bot cannot mention <@&%s>: role is not mentionable and bot lacks Mention @everyone, @here, and All Roles permission in <#%s>", roleID, channelID)
		}
	}
	return fmt.Errorf("role <@&%s> was not found in this server", roleID)
}

type allowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
}

type createMessageRequest struct {
	Content         string           `json:"content"`
	AllowedMentions *allowedMentions `json:"allowed_mentions,omitempty"`
}

// SendChannelMessage posts content to channelID, explicitly scoping mentions
// to roleID when non-empty.
func (c *Client) SendChannelMessage(ctx context.Context, channelID, content, roleID string) error {
	channelID = strings.TrimSpace(channelID)
	roleID = strings.TrimSpace(roleID)
	if channelID == "" {
		return errors.New("discord channel id cannot be empty")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("discord message content cannot be empty")
	}

	am := &allowedMentions{Parse: []string{}}
	if roleID != "" {
		am.Roles = []string{roleID}
	}

	endpoint := fmt.Sprintf("%s/channels/%s/messages", c.baseURL, channelID)
	return c.doJSON(ctx, http.MethodPost, endpoint, createMessageRequest{
		Content:         content,
		AllowedMentions: am,
	}, nil)
}

func (c *Client) doJSON(ctx context.Context, method, url string, reqBody, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal discord request: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("build discord request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+c.botToken)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute discord request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read discord response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if respOut != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, respOut); err != nil {
			return fmt.Errorf("decode discord response: %w", err)
		}
	}
	return nil
}

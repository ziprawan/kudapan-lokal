package discordbot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"kudapan_lokal/internal/mdbdiff"
	"kudapan_lokal/internal/webhook"
)

const discordFlagEphemeral = 1 << 6

// PermissionVerifier validates whether the bot can post to a target Discord
// channel and optionally mention a target role.
type PermissionVerifier interface {
	VerifyNotifyPermissions(ctx context.Context, guildID, channelID, roleID string, ix webhook.DiscordInteraction) error
}

// RegisterCommands attaches Discord slash-command handlers (/notify-event and
// /notify-resource) to handler backed by store.
func RegisterCommands(
	handler *webhook.DiscordHandler,
	store *Store,
	verifier PermissionVerifier,
	masterDBPath string,
	nowFn func() time.Time,
) {
	if handler == nil || store == nil {
		return
	}
	if nowFn == nil {
		nowFn = time.Now
	}

	registerNotifyCommand(handler, "notify-event", verifier, func(ctx context.Context, guildID, channelID, roleID string) string {
		if strings.TrimSpace(masterDBPath) != "" {
			maxPastID, err := mdbdiff.MaxPastChampionsRoundMilestoneID(masterDBPath, nowFn().UTC())
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Sprintf("Failed to inspect Champions Meeting schedule: %v", err)
			}
			if _, err := store.EnsureLastNotifiedID(ctx, EventTypeChampionsMeeting, maxPastID); err != nil {
				return fmt.Sprintf("Failed to initialize event tracker: %v", err)
			}
		}

		if _, err := store.UpsertGuild(ctx, guildID, channelID, roleID); err != nil {
			return fmt.Sprintf("Failed to save notification channel: %v", err)
		}

		if roleID != "" {
			return fmt.Sprintf("Champions Meeting notifications will be sent to <#%s> (tagging <@&%s>).", channelID, roleID)
		}
		return fmt.Sprintf("Champions Meeting notifications will be sent to <#%s>.", channelID)
	})

	registerNotifyCommand(handler, "notify-resource", verifier, func(ctx context.Context, guildID, channelID, roleID string) string {
		if _, err := store.UpsertResourceGuild(ctx, guildID, channelID, roleID); err != nil {
			return fmt.Sprintf("Failed to save resource notification channel: %v", err)
		}

		if roleID != "" {
			return fmt.Sprintf("Game resource update notifications will be sent to <#%s> (tagging <@&%s>).", channelID, roleID)
		}
		return fmt.Sprintf("Game resource update notifications will be sent to <#%s>.", channelID)
	})
}

func registerNotifyCommand(
	handler *webhook.DiscordHandler,
	name string,
	verifier PermissionVerifier,
	onVerified func(ctx context.Context, guildID, channelID, roleID string) string,
) {
	handler.RegisterCommand(name, func(ctx context.Context, ix webhook.DiscordInteraction) webhook.DiscordInteractionResponse {
		if strings.TrimSpace(ix.GuildID) == "" {
			return ephemeralReply("This command can only be used inside a Discord server.")
		}
		if ix.Member == nil || !HasAdminPermission(ix.Member.Permissions) {
			return ephemeralReply("Only server administrators can use this command.")
		}

		channelID, ok := ix.Data.StringOption("channel")
		if !ok || strings.TrimSpace(channelID) == "" {
			return ephemeralReply("Missing required `channel` option.")
		}
		channelID = strings.TrimSpace(channelID)

		roleID, _ := ix.Data.StringOption("role")
		roleID = strings.TrimSpace(roleID)

		if verifier != nil {
			if err := verifier.VerifyNotifyPermissions(ctx, ix.GuildID, channelID, roleID, ix); err != nil {
				return ephemeralReply(fmt.Sprintf("Permission check failed: %v", err))
			}
		}

		return ephemeralReply(onVerified(ctx, ix.GuildID, channelID, roleID))
	})
}

func ephemeralReply(msg string) webhook.DiscordInteractionResponse {
	return webhook.DiscordInteractionResponse{
		Type: webhook.DiscordResponseChannelMessageWithSource,
		Data: &webhook.DiscordInteractionResponseData{
			Content: msg,
			Flags:   discordFlagEphemeral,
		},
	}
}

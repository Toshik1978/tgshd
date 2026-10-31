package telegram

import (
	"context"

	"go.uber.org/zap"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// commandSetter is the part of the Telegram bot API that writes the command
// menu. It is satisfied by *telebot.Bot.
type commandSetter interface {
	SetCommands(opts ...any) error
	DeleteCommands(opts ...any) error
}

// publishMenu removes the default command menu so that only allowed users see
// commands, then publishes the menu to every allowed scope. A failure is
// logged and does not stop publishing; a cancelled context does.
func (c *consumer) publishMenu(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	scope := telebot.CommandScope{Type: telebot.CommandScopeDefault}
	if err := c.setter.DeleteCommands(scope); err != nil {
		c.logger.Warn("Failed to delete default command menu", zap.String("scope", scope.Type), zap.Error(err))
	}

	if len(c.menu) == 0 {
		return
	}

	cmds := telebotCommands(c.menu)
	published := 0
	for _, ms := range c.access.MenuScopes() {
		if ctx.Err() != nil {
			return
		}

		sc := telebotScope(ms)
		if err := c.setter.SetCommands(cmds, sc); err != nil {
			c.logger.Warn("Failed to set command menu",
				zap.String("scope", sc.Type), zap.Int64("chat_id", sc.ChatID),
				zap.Int64("user_id", sc.UserID), zap.Error(err))

			continue
		}
		published++
	}

	c.logger.Info("Published command menu", zap.Int("scopes", published))
}

// telebotCommands converts the menu to the telebot representation.
func telebotCommands(menu []route.BotCommand) []telebot.Command {
	cmds := make([]telebot.Command, len(menu))
	for i, m := range menu {
		cmds[i] = telebot.Command{Text: m.Name, Description: m.Description}
	}

	return cmds
}

// telebotScope converts a menu scope to the only two Telegram scopes ever
// written: a private chat or one member of one group.
func telebotScope(ms route.MenuScope) telebot.CommandScope {
	if ms.Kind == route.ScopeGroupMember {
		return telebot.CommandScope{Type: telebot.CommandScopeChatMember, ChatID: ms.ChatID, UserID: ms.UserID}
	}

	return telebot.CommandScope{Type: telebot.CommandScopeChat, ChatID: ms.ChatID}
}

package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"reasonix/internal/bot"
	"reasonix/internal/config"
)

// sendMessage 发送一条出站消息到 Telegram。
func (a *adapter) sendMessage(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	chatID, err := parseChatID(msg.ChatID)
	if err != nil {
		return bot.SendResult{}, fmt.Errorf("telegram send: %w", err)
	}

	// 如果有内联键盘，使用含键盘的消息
	if msg.Keyboard != nil {
		return a.sendWithKeyboard(ctx, chatID, msg)
	}

	// 普通文本消息
	text := msg.Text
	if text == "" {
		return bot.SendResult{}, nil
	}

	// 长消息拆分发送
	parts := splitMessage(text)
	if len(parts) == 0 {
		return bot.SendResult{}, nil
	}

	var result bot.SendResult
	for i, part := range parts {
		tgMsg := tgbotapi.NewMessage(chatID, part)
		tgMsg.ParseMode = "MarkdownV2"
		tgMsg.DisableWebPagePreview = true

		if i == 0 && msg.ReplyToMsgID != "" {
			if replyID, err := parseMessageID(msg.ReplyToMsgID); err == nil {
				tgMsg.ReplyToMessageID = replyID
			}
		}

		sent, err := a.bot.Send(tgMsg)
		if err != nil {
			// MarkdownV2 渲染失败时回退到纯文本
			tgMsg.ParseMode = ""
			sent, err = a.bot.Send(tgMsg)
			if err != nil {
				return result, fmt.Errorf("telegram send part %d: %w", i, err)
			}
		}

		result.Merge(bot.SendResult{
			MessageID: fmt.Sprintf("%d", sent.MessageID),
		})
	}

	return result, nil
}

// sendWithKeyboard 发送带内联键盘的消息。
func (a *adapter) sendWithKeyboard(ctx context.Context, chatID int64, msg bot.OutboundMessage) (bot.SendResult, error) {
	text := msg.Text
	if text == "" {
		text = "需要您的确认："
	}

	tgMsg := tgbotapi.NewMessage(chatID, text)
	tgMsg.ParseMode = "MarkdownV2"

	// 构建内联键盘
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, row := range msg.Keyboard.Rows {
		var tgRow []tgbotapi.InlineKeyboardButton
		for _, btn := range row.Buttons {
			tgBtn := tgbotapi.NewInlineKeyboardButtonData(btn.Label, btn.ID)
			tgRow = append(tgRow, tgBtn)
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgRow...))
	}
	tgMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	sent, err := a.bot.Send(tgMsg)
	if err != nil {
		// MarkdownV2 渲染失败时回退到纯文本
		tgMsg.ParseMode = ""
		sent, err = a.bot.Send(tgMsg)
		if err != nil {
			return bot.SendResult{}, fmt.Errorf("telegram send keyboard: %w", err)
		}
	}

	return bot.SendResult{
		MessageID: fmt.Sprintf("%d", sent.MessageID),
	}, nil
}

// sendTyping 发送"正在输入"状态。
func (a *adapter) sendTyping(ctx context.Context, chatID string) error {
	cid, err := parseChatID(chatID)
	if err != nil {
		return err
	}

	action := tgbotapi.NewChatAction(cid, tgbotapi.ChatTyping)
	if _, err := a.bot.Request(action); err != nil {
		return fmt.Errorf("telegram send typing: %w", err)
	}
	return nil
}

// parseChatID 将字符串 ChatID 转换为 int64。
func parseChatID(chatID string) (int64, error) {
	var cid int64
	if _, err := fmt.Sscanf(chatID, "%d", &cid); err != nil {
		return 0, fmt.Errorf("invalid chat id %q: %w", chatID, err)
	}
	return cid, nil
}

// parseMessageID 将字符串 MessageID 转换为 int。
func parseMessageID(msgID string) (int, error) {
	var mid int
	if _, err := fmt.Sscanf(msgID, "%d", &mid); err != nil {
		return 0, fmt.Errorf("invalid message id %q: %w", msgID, err)
	}
	return mid, nil
}

// SendText 发送一条纯文本消息到指定 Telegram 聊天（用于外部测试）。
func SendText(ctx context.Context, tgCfg config.TelegramBotConfig, chatID, text string) (bot.SendResult, error) {
	a := &adapter{
		cfg:    tgCfg,
		logger: slog.Default().With("platform", "telegram"),
	}
	token := os.Getenv(tgCfg.TokenEnv)
	if token == "" {
		return bot.SendResult{}, fmt.Errorf("telegram: %s not set", tgCfg.TokenEnv)
	}
	tgbot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return bot.SendResult{}, fmt.Errorf("telegram: failed to create bot: %w", err)
	}
	a.bot = tgbot
	return a.sendMessage(ctx, bot.OutboundMessage{ChatID: chatID, Text: text})
}

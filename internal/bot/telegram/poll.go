package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"reasonix/internal/bot"
)

const (
	pollInterval     = 300 * time.Millisecond // 轮询间隔
	pollTimeout      = 60                     // Telegram long polling timeout (seconds)
	pollRetryDelay   = 3 * time.Second        // 出错后重试等待
	maxTextLength    = 4000                   // Telegram 单条消息最大长度（留 96 字节余量）
)

// pollLoop 长轮询 Telegram 获取更新。
func (a *adapter) pollLoop(ctx context.Context) {
	a.logger.Info("telegram polling started")

	// 等待 bot 完全初始化
	if !bot.SleepCtx(ctx, 1*time.Second) {
		return
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = pollTimeout
	u.AllowedUpdates = a.allowedUpdates

	updates := a.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("telegram polling stopped")
			return
		case upd, ok := <-updates:
			if !ok {
				a.logger.Warn("telegram update channel closed, restarting polling")
				if !bot.SleepCtx(ctx, pollRetryDelay) {
					return
				}
				// 重新获取更新通道
				u.Offset = a.lastOffset + 1
				updates = a.bot.GetUpdatesChan(u)
				continue
			}

			a.lastOffset = upd.UpdateID

			if upd.Message != nil {
				a.handleMessage(upd.Message)
			} else if upd.CallbackQuery != nil {
				a.handleCallbackQuery(upd.CallbackQuery)
			}
		}
	}
}

// handleMessage 处理单条入站消息。
func (a *adapter) handleMessage(msg *tgbotapi.Message) {
	// 忽略 bot 自己的消息
	if a.isSelfMessage(msg.From.ID) {
		return
	}

	// 忽略旧消息（5分钟以前）—— 防止启动时发送大量历史消息
	if msg.Time().Before(time.Now().Add(-5 * time.Minute)) {
		a.logger.Debug("ignoring stale message", "time", msg.Time())
		return
	}

	chatType := chatTypeFromTG(string(msg.Chat.Type))
	text := extractText(msg, a.bot.Self.UserName)

	// 群聊中如果不是 @bot 或没有文本，则忽略
	if chatType == bot.ChatGroup && text == "" && !a.isMentioningBot(msg) {
		return
	}

	if text == "" && len(msg.Photo) == 0 && msg.Document == nil {
		return // 无文字无媒体，忽略
	}

	ib := bot.InboundMessage{
		Platform:  bot.PlatformTelegram,
		ChatType:  chatType,
		ChatID:    chatIDStr(msg.Chat.ID),
		UserID:    userIDStr(msg.From.ID),
		UserName:  msg.From.UserName,
		Text:      text,
		MessageID: fmt.Sprintf("%d", msg.MessageID),
	}

	// 处理媒体附件
	if len(msg.Photo) > 0 {
		// 取最大尺寸的照片
		photo := msg.Photo[len(msg.Photo)-1]
		if photo.FileID != "" {
			ib.MediaURLs = append(ib.MediaURLs, photo.FileID)
		}
	}
	if msg.Document != nil && msg.Document.FileID != "" {
		ib.MediaURLs = append(ib.MediaURLs, msg.Document.FileID)
	}
	if msg.Voice != nil && msg.Voice.FileID != "" {
		ib.MediaURLs = append(ib.MediaURLs, msg.Voice.FileID)
	}
	if msg.Audio != nil && msg.Audio.FileID != "" {
		ib.MediaURLs = append(ib.MediaURLs, msg.Audio.FileID)
	}
	if msg.Video != nil && msg.Video.FileID != "" {
		ib.MediaURLs = append(ib.MediaURLs, msg.Video.FileID)
	}

	// 回复消息：引用原文
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
		ib.Text = fmt.Sprintf("> %s\n\n%s", msg.ReplyToMessage.Text, ib.Text)
	}

	select {
	case a.msgCh <- ib:
		a.logger.Debug("telegram inbound queued",
			"chat_type", chatType,
			"chat", logHash(ib.ChatID),
			"user", logHash(ib.UserID),
			"text_chars", len([]rune(ib.Text)),
			"media", len(ib.MediaURLs))
	default:
		a.logger.Warn("telegram message channel full, dropping message")
	}
}

// handleCallbackQuery 处理内联键盘回调。
func (a *adapter) handleCallbackQuery(cq *tgbotapi.CallbackQuery) {
	a.logger.Debug("telegram callback query",
		"from", cq.From.UserName,
		"data", cq.Data)

	// 应答回调（停止 loading）
	callbackResp := tgbotapi.NewCallback(cq.ID, "")
	if _, err := a.bot.Request(callbackResp); err != nil {
		a.logger.Warn("telegram callback answer failed", "err", err)
	}

	// 将 callback 构造为入站消息，方便 gateway 通过 inline keyboard 处理审批
	ib := bot.InboundMessage{
		Platform:  bot.PlatformTelegram,
		ChatType:  bot.ChatDM,
		ChatID:    chatIDStr(cq.Message.Chat.ID),
		UserID:    userIDStr(cq.From.ID),
		UserName:  cq.From.UserName,
		Text:      cq.Data, // callback data 作为文本内容
		MessageID: cq.ID,
		// 标记这个消息是一个 callback（键盘回复）而非普通消息
	}

	// 如果 callback 来自群聊
	if cq.Message.Chat.IsGroup() || cq.Message.Chat.IsSuperGroup() {
		ib.ChatType = bot.ChatGroup
	}

	select {
	case a.msgCh <- ib:
		a.logger.Debug("telegram callback queued",
			"chat", logHash(ib.ChatID),
			"user", logHash(ib.UserID),
			"data", cq.Data)
	default:
		a.logger.Warn("telegram callback channel full")
	}
}

// logHash 对 ID 进行哈希截断用于日志。
func logHash(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// splitMessage 将长文本按 maxTextLength 拆分为多条。
func splitMessage(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	runes := []rune(text)
	if len(runes) <= maxTextLength {
		return []string{text}
	}

	var parts []string
	for len(runes) > 0 {
		if len(runes) <= maxTextLength {
			parts = append(parts, string(runes))
			break
		}
		// 在 maxTextLength 内找最后一个换行或空格截断
		cut := maxTextLength
		for cut > maxTextLength/2 {
			if runes[cut] == '\n' || runes[cut] == ' ' {
				break
			}
			cut--
		}
		if cut <= maxTextLength/2 {
			cut = maxTextLength // 没找到合适截断点，硬切
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	return parts
}

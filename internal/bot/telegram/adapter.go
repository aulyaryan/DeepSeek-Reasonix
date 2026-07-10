// Package telegram 实现 Telegram Bot 适配器。
// 使用 go-telegram-bot-api 库，通过长轮询（Long Polling）接收消息。
// 架构参考 Hermes Agent 的 Telegram gateway/adapter 模式：
// - 长轮询 getUpdates
// - 消息发送（纯文本 + Markdown）
// - inline keyboard 审批
// - typing 状态指示器
// - DM / group / topic 支持
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"reasonix/internal/bot"
	"reasonix/internal/config"
)

// DefaultAllowedUpdates 默认允许的 Telegram 更新类型。
var DefaultAllowedUpdates = []string{"message", "callback_query"}

// New 创建 Telegram Bot 适配器。
func New(cfg config.TelegramBotConfig, logger *slog.Logger) bot.Adapter {
	allowed := cfg.AllowedUpdates
	if len(allowed) == 0 {
		allowed = DefaultAllowedUpdates
	}
	return &adapter{
		cfg:            cfg,
		logger:         logger.With("platform", "telegram"),
		allowedUpdates: allowed,
	}
}

type adapter struct {
	cfg    config.TelegramBotConfig
	logger *slog.Logger
	msgCh  chan bot.InboundMessage
	cancel context.CancelFunc

	bot   *tgbotapi.BotAPI
	botID int64 // bot 自身的 user ID，用于群聊 @门控

	allowedUpdates []string

	// sendMu 序列化出站请求，避免 Telegram 速率限制
	sendMu     sync.Mutex
	lastOffset int // 最后一个已处理的 update_id

	// callbackData caches mapping from callback query IDs to chat IDs for inline
	// keyboard approval routing.
	callbackMu sync.Mutex
	callbacks  map[string]callbackMeta
}

// callbackMeta stores the context needed to route a callback query back to
// the original chat session.
type callbackMeta struct {
	ChatID  string
	ChatType bot.ChatType
	UserID  string
	MessageID string
}

func (a *adapter) Platform() bot.Platform { return bot.PlatformTelegram }
func (a *adapter) Name() string           { return "telegram" }

func (a *adapter) Start(ctx context.Context) error {
	a.msgCh = make(chan bot.InboundMessage, 64)
	a.callbacks = make(map[string]callbackMeta)

	token := os.Getenv(a.cfg.TokenEnv)
	if token == "" {
		return fmt.Errorf("telegram: %s is not set", a.cfg.TokenEnv)
	}

	tgbot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return fmt.Errorf("telegram: failed to create bot: %w", err)
	}

	// 可选：设置自定义 HTTP 客户端（用于代理等）
	// tgbot.Client = customHTTPClient

	a.bot = tgbot
	a.botID = tgbot.Self.ID

	a.logger.Info("telegram bot started",
		"username", tgbot.Self.UserName,
		"bot_id", a.botID,
		"allowed_updates", a.allowedUpdates)

	// 启动长轮询
	ctx, a.cancel = context.WithCancel(ctx)
	go a.pollLoop(ctx)

	return nil
}

func (a *adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.bot != nil {
		a.bot.StopReceivingUpdates()
	}
	return nil
}

func (a *adapter) Send(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	return a.sendMessage(ctx, msg)
}

func (a *adapter) SendTyping(ctx context.Context, chatID string) error {
	return a.sendTyping(ctx, chatID)
}

func (a *adapter) Messages() <-chan bot.InboundMessage {
	return a.msgCh
}

// botToken 返回处理过的 token（仅用于日志中截断显示）
func (a *adapter) botTokenPreview() string {
	token := os.Getenv(a.cfg.TokenEnv)
	if len(token) > 8 {
		return token[:4] + "..." + token[len(token)-4:]
	}
	return "***"
}

// isSelfMessage 检查消息是否由 bot 自己发出（用于回声抑制）
func (a *adapter) isSelfMessage(userID int64) bool {
	return userID == a.botID
}

// chatTypeFromTG 将 Telegram 聊天类型映射到内部 ChatType。
func chatTypeFromTG(tgChatType string) bot.ChatType {
	switch tgChatType {
	case "private":
		return bot.ChatDM
	case "group":
		return bot.ChatGroup
	case "supergroup":
		return bot.ChatGroup
	case "channel":
		return bot.ChatDirect
	default:
		return bot.ChatDM
	}
}

// chatIDStr 返回聊天的字符串 ID（用于内部路由）。
func chatIDStr(chatID int64) string {
	return fmt.Sprintf("%d", chatID)
}

// userIDStr 返回用户的字符串 ID。
func userIDStr(userID int64) string {
	return fmt.Sprintf("%d", userID)
}

// isMentioningBot 检查消息是否 @ 提到了 bot（用于群聊门控）。
func (a *adapter) isMentioningBot(msg *tgbotapi.Message) bool {
	if msg == nil || msg.Text == "" {
		return false
	}
	runes := []rune(msg.Text)
	for _, e := range msg.Entities {
		if e.Type == "mention" && e.Offset >= 0 && e.Length > 0 {
			end := e.Offset + e.Length
			if end > len(runes) {
				end = len(runes)
			}
			mention := string(runes[e.Offset:end])
			mention = strings.TrimPrefix(mention, "@")
			if strings.EqualFold(mention, a.bot.Self.UserName) {
				return true
			}
		}
	}
	return false
}

// extractText 从消息中提取纯文本（去除 @bot 提及前缀）。
func extractText(msg *tgbotapi.Message, botUserName string) string {
	if msg.Text != "" {
		text := msg.Text
		// 如果是群聊且消息以 @bot 开头，去掉提及
		if msg.Chat.IsGroup() || msg.Chat.IsSuperGroup() {
			parts := strings.SplitN(text, " ", 2)
			if len(parts) >= 1 && strings.HasPrefix(parts[0], "@") {
				mention := strings.TrimPrefix(parts[0], "@")
				if strings.EqualFold(mention, botUserName) {
					if len(parts) > 1 {
						text = parts[1]
					} else {
						text = ""
					}
				}
			}
		}
		return text
	}
	if msg.Caption != "" {
		return msg.Caption
	}
	return ""
}

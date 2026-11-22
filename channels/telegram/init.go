package telegram

import (
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
)

func init() {
	channels.RegisterTelegramBotFactory(func(botCfg config.TelegramBotConfig, b *bus.MessageBus) (channels.Channel, error) {
		return NewTelegramChannelFromConfig(botCfg, b)
	})
}

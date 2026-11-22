package discord

import (
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
)

func init() {
	channels.RegisterFactory("discord", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewDiscordChannel(cfg.Channels.Discord, b)
	})
	// The channel reads nothing but its own section.
	channels.RegisterFactoryInputs("discord", func(cfg *config.Config) any { return cfg.Channels.Discord })
}

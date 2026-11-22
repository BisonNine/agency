package slack

import (
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
)

func init() {
	channels.RegisterFactory("slack", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewSlackChannel(cfg.Channels.Slack, b)
	})
	// The channel reads nothing but its own section.
	channels.RegisterFactoryInputs("slack", func(cfg *config.Config) any { return cfg.Channels.Slack })
}

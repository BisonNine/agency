package matrix

import (
	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/channels"
	"go.bison9.com/agency/config"
)

func init() {
	channels.RegisterFactory("matrix", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewMatrixChannel(cfg.Channels.Matrix, b)
	})
	// The channel reads nothing but its own section.
	channels.RegisterFactoryInputs("matrix", func(cfg *config.Config) any { return cfg.Channels.Matrix })
}

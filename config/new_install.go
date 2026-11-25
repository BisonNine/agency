package config

import (
	"crypto/rand"
	"encoding/hex"
)

// NewInstallConfig is the configuration of a new install: DefaultConfig with
// the built-in WebUI chat channel switched on, so a gateway started on it has
// a channel and stays up. The channel's token is generated here, each time,
// from crypto/rand; DefaultConfig itself stays fixed (it is also the base
// every existing file is loaded over, and an existing install keeps its own
// channels.webui settings).
func NewInstallConfig() *Config {
	cfg := DefaultConfig()
	cfg.Channels.WebUI.Enabled = true
	cfg.Channels.WebUI.Token = newWebUIToken()
	// Without allow_from the channel drops every message.
	cfg.Channels.WebUI.AllowFrom = FlexibleStringSlice{"*"}
	return cfg
}

// newWebUIToken is a random 32-character hex token. crypto/rand.Read never
// fails (it panics where no randomness can be had).
func newWebUIToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

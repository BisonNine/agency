package secmsg

import (
	"go.bison9.com/agency/channels"
)

func init() {
	channels.RegisterSecMsgFactory(NewFromConfig)
	channels.RegisterSecMsgDiscovery(DiscoverAccounts)
}

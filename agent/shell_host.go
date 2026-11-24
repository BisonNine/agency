// Agency
// License: MIT

package agent

import (
	"context"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/routing"
	toolsshell "go.bison9.com/agency/tools/shell"
)

// shellNoticeSender is the sender of a shell job notice that no call
// carries, the same as a job notice delivered through the call's callback.
const shellNoticeSender = "async:shell_exec"

// ShellHooks are what the shell tools' mount needs from the loop: the turn
// id for its audit rows, and the delivery of a notice no call carries (a job
// lost when the service restarted).
func (al *AgentLoop) ShellHooks() toolsshell.Hooks {
	return toolsshell.Hooks{TurnID: turnIDFrom, Deliver: al.deliverShellNotice}
}

// deliverShellNotice queues text as a background result for owner's main
// conversation. The chat the job was started from is not known after a
// restart: as for a forum's notice then, the agent's answer goes to its
// default chat (its default binding) when it has one, and otherwise stays
// in the conversation.
func (al *AgentLoop) deliverShellNotice(owner, text string) {
	channel, chatID := constants.AgentMessageChannel, ""
	if cfg := al.GetConfig(); cfg != nil {
		if c, id, _, ok := cfg.CronTarget(owner); ok {
			channel, chatID = c, id
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := al.bus.PublishInbound(ctx, bus.InboundMessage{
		Channel:    "system",
		SenderID:   shellNoticeSender,
		ChatID:     chatID,
		Content:    text,
		SessionKey: routing.BuildAgentMainSessionKey(owner),
		Metadata: map[string]string{
			metadataKeyPreresolvedAgentID: owner,
			bus.MetaOriginChannel:         channel,
		},
		Internal: true,
	}); err != nil {
		logger.WarnCF("agent", "Shell job notice not queued", map[string]any{"agent_id": owner, "error": err.Error()})
	}
}

// ClawEh
// License: MIT

package agent

import (
	"context"

	"go.bison9.com/agent/loop"

	"go.bison9.com/agency/bus"
	"go.bison9.com/agency/constants"
	"go.bison9.com/agency/logger"
	"go.bison9.com/agency/providers"
	"go.bison9.com/agency/tools"
	toolsagents "go.bison9.com/agency/tools/agents"
)

// publishBreadcrumb posts the one-line, privacy-safe note for a tool call.
func (al *AgentLoop) publishBreadcrumb(ctx context.Context, opts processOptions, tc providers.ToolCall) {
	line := toolCallBreadcrumb(tc)
	if line == "" {
		return
	}
	bcCtx, bcCancel := context.WithTimeout(ctx, publishTimeout)
	defer bcCancel()
	if err := al.bus.PublishOutbound(bcCtx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: line,
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish tool breadcrumb",
			map[string]any{"error": err.Error(), "channel": opts.Channel})
	}
}

// asyncToolCallback is the completion callback of an AsyncExecutor tool. Its
// ForUser output goes straight to the user, as on the synchronous path; its
// result re-enters the agent loop as a "system" message so
// processSystemMessage routes it back through a normal turn. It is built when
// the late result arrives, after the turn and perhaps after the agent; where
// the result goes reads only the agent's Spec, which never changes: a clone's
// late result goes to its source's main conversation, as a sub-agent's always
// has.
func (al *AgentLoop) asyncToolCallback(ctx context.Context, agent *AgentInstance, opts processOptions, toolName string) tools.AsyncCallback {
	resultAgentID, resultSessionKey := asyncResultTarget(agent)
	turnDepth := toolsagents.SpawnDepth(ctx)
	return func(cbCtx context.Context, result *tools.ToolResult) {
		// An asked turn has no chat: its user-facing output is dropped.
		if !result.Silent && result.ForUser != "" && opts.Channel != constants.AgentMessageChannel {
			al.deliverAsyncForUser(cbCtx, opts, toolName, result.ForUser)
		}

		content := result.ForLLM
		if content == "" && result.Err != nil {
			content = result.Err.Error()
		}
		if content == "" {
			return
		}
		content = loop.CapToolResult(content, result.IsError, al.sessionLimits(agent, opts.SessionKey).contextWindow)

		logger.InfoCF("agent", "Async tool completed, publishing result",
			map[string]any{
				"tool":        toolName,
				"content_len": len(content),
				"channel":     opts.Channel,
			})

		pubCtx, pubCancel := context.WithTimeout(context.WithoutCancel(cbCtx), publishTimeout)
		defer pubCancel()
		if err := al.bus.PublishInbound(pubCtx, bus.InboundMessage{
			Channel:    "system",
			SenderID:   "async:" + toolName,
			Internal:   true,
			ChatID:     opts.ChatID,
			Content:    content,
			SessionKey: resultSessionKey,
			// The re-entered turn runs at this turn's depth, never lower.
			Metadata: bus.SetSpawnDepth(map[string]string{
				metadataKeyPreresolvedAgentID: resultAgentID, bus.MetaOriginChannel: opts.Channel,
			}, turnDepth),
		}); err != nil {
			logger.WarnCF("agent", "Failed to deliver async tool result to agent",
				map[string]any{"error": err.Error(), "tool": toolName, "session": opts.SessionKey})
		}
	}
}

// deliverAsyncForUser sends an async tool's ForUser output to the chat of
// the turn that started it.
func (al *AgentLoop) deliverAsyncForUser(cbCtx context.Context, opts processOptions, toolName, forUser string) {
	outCtx, outCancel := context.WithTimeout(context.WithoutCancel(cbCtx), publishTimeout)
	defer outCancel()
	logger.InfoCF("agent", "Async tool completed, delivering to user",
		map[string]any{
			"tool":        toolName,
			"channel":     opts.Channel,
			"chat_id":     opts.ChatID,
			"content_len": len(forUser),
		})
	if err := al.bus.PublishOutbound(outCtx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: forUser,
	}); err != nil {
		logger.WarnCF("agent", "Failed to deliver async tool result to user",
			map[string]any{"error": err.Error(), "tool": toolName, "channel": opts.Channel})
	}
}

// publishToolForUser sends a tool's ForUser output to the chat.
func (al *AgentLoop) publishToolForUser(ctx context.Context, opts processOptions, tool string, result *tools.ToolResult) {
	if err := al.bus.PublishOutbound(ctx, bus.OutboundMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Content: result.ForUser,
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish tool ForUser content",
			map[string]any{
				"tool":    tool,
				"channel": opts.Channel,
				"error":   err.Error(),
			})
		return
	}
	logger.DebugCF("agent", "Sent tool result to user",
		map[string]any{
			"tool":        tool,
			"content_len": len(result.ForUser),
		})
}

// publishToolMedia sends the media refs a tool returned to the chat.
func (al *AgentLoop) publishToolMedia(ctx context.Context, opts processOptions, tool string, result *tools.ToolResult) {
	parts := make([]bus.MediaPart, 0, len(result.Media))
	for _, ref := range result.Media {
		part := bus.MediaPart{Ref: ref}
		if al.mediaStore != nil {
			if _, meta, err := al.mediaStore.ResolveWithMeta(ref); err == nil {
				part.Filename = meta.Filename
				part.ContentType = meta.ContentType
				part.Type = inferMediaType(meta.Filename, meta.ContentType)
			}
		}
		parts = append(parts, part)
	}
	if err := al.bus.PublishOutboundMedia(ctx, bus.OutboundMediaMessage{
		Channel: opts.Channel,
		ChatID:  opts.ChatID,
		Parts:   parts,
	}); err != nil {
		logger.WarnCF("agent", "Failed to publish tool media",
			map[string]any{
				"tool":    tool,
				"channel": opts.Channel,
				"error":   err.Error(),
			})
	}
}

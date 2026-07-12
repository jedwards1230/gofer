package runner

import (
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// projectMessages folds the journal's current context and projects it down
// to the provider message model Request.Messages speaks: a plain entry
// becomes one message (text, plus a reasoning block when present); a
// tool-round entry becomes the pair of messages a provider expects — an
// assistant message carrying one ToolUseBlock per call, followed by a user
// message carrying the matching ToolResultBlocks.
func (r *Runner) projectMessages() []provider.Message {
	fold := r.journal.Fold()
	msgs := make([]provider.Message, 0, len(fold))
	for _, cm := range fold {
		if len(cm.ToolCalls) == 0 {
			msgs = append(msgs, projectPlain(cm))
			continue
		}
		msgs = append(msgs, projectToolRound(cm.ToolCalls)...)
	}
	return msgs
}

// projectPlain projects a plain (non-tool-round) folded message.
func projectPlain(cm session.ContextMessage) provider.Message {
	blocks := []provider.ContentBlock{provider.TextBlock(cm.Content)}
	if cm.Reasoning != "" {
		blocks = append(blocks, provider.ReasoningBlock(cm.Reasoning))
	}
	return provider.Message{Role: mapRole(cm.Role), Content: blocks}
}

// projectToolRound projects a tool-round's calls into the assistant/user
// message pair a provider expects: the assistant's tool_use blocks, then the
// matching tool_result blocks as a user message.
func projectToolRound(calls []session.ToolCallRecord) []provider.Message {
	assistantBlocks := make([]provider.ContentBlock, 0, len(calls))
	resultBlocks := make([]provider.ContentBlock, 0, len(calls))
	for _, tc := range calls {
		assistantBlocks = append(assistantBlocks, provider.ToolUseBlock(tc.ID, tc.Name, tc.Input))
		resultBlocks = append(resultBlocks, provider.ToolResultBlock(tc.ID, tc.Result, tc.IsError))
	}
	return []provider.Message{
		{Role: provider.RoleAssistant, Content: assistantBlocks},
		{Role: provider.RoleUser, Content: resultBlocks},
	}
}

// mapRole maps a journal entry's string role onto the provider's typed Role,
// defaulting unrecognized roles to RoleUser.
func mapRole(role string) provider.Role {
	switch role {
	case "assistant":
		return provider.RoleAssistant
	case "system":
		return provider.RoleSystem
	default:
		return provider.RoleUser
	}
}

package daemon

import (
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// historyEvents projects sessionID's folded conversation history (provider
// messages) into the [event.Event] stream a gofer/event history replay
// carries on session/load — the gofer-native counterpart to
// [acp.ReplayNotifications], mirroring its role/block handling exactly (see
// that function's doc) but emitting event.Event values instead of ACP
// session/update notifications, so a gofer client (internal/daemonbridge),
// which ignores session/update entirely as of the M3 lossless-attach work,
// still gets a full history replay via gofer/event (see handleSessionLoad).
//
// As with ReplayNotifications, this is only as lossless as the persisted
// journal allows: intermediate live deltas were never persisted, so each
// message here is a single MessageStarted/MessageFinished pair carrying the
// block's settled content — not the delta-by-delta stream a live turn
// produced — the same replay ceiling ACP's history projection has. The
// returned slice is always non-nil.
func historyEvents(sessionID string, msgs []provider.Message) []event.Event {
	events := make([]event.Event, 0, len(msgs))

	for _, msg := range msgs {
		if msg.Role == provider.RoleSystem {
			continue
		}

		for _, b := range msg.Content {
			switch b.Type {
			case provider.BlockText:
				if b.Text == "" {
					continue
				}
				if msg.Role == provider.RoleUser {
					events = append(events,
						event.NewMessageStarted(sessionID, event.MessageUser),
						event.NewMessageFinished(sessionID, event.MessageUser, b.Text),
					)
				} else {
					events = append(events,
						event.NewMessageStarted(sessionID, event.MessageText),
						event.NewMessageFinishedMeta(sessionID, event.MessageText, b.Text, b.Meta),
					)
				}

			case provider.BlockReasoning:
				if b.Text == "" {
					continue
				}
				events = append(events,
					event.NewMessageStarted(sessionID, event.MessageReasoning),
					event.NewMessageFinishedMeta(sessionID, event.MessageReasoning, b.Text, b.Meta),
				)

			case provider.BlockToolUse:
				events = append(events, event.NewToolCallStarted(sessionID, b.ToolUseID, b.ToolName, b.ToolInput))

			case provider.BlockToolResult:
				events = append(events, event.NewToolCallFinished(sessionID, b.ToolUseID, nil, b.ToolResult, b.IsError, nil))

			case provider.BlockImage:
				// M1 placeholder; unmodeled — mirrors acp.ReplayNotifications.
			}
		}
	}

	return events
}

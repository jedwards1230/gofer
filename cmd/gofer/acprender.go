package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// ANSI SGR codes used to dim reasoning output when color is enabled —
// mirrors internal/render's Human renderer, duplicated rather than exported
// since this renderer is deliberately its own small thing (see acpRenderer's
// doc).
const (
	acpAnsiDim   = "\x1b[2m"
	acpAnsiReset = "\x1b[0m"
)

// acpUpdateWire is the decode shape of a session/update notification's
// params — [acp.SessionNotification]'s wire form. acp has no client-side
// decoder for it (it is written for gofer to play the agent role, not the
// client role — see internal/daemon/prompt_test.go's sessionUpdateParams,
// which takes the same approach for the same reason), so this decodes the
// small, closed set of variants the daemon emits: agent_message_chunk,
// agent_thought_chunk, tool_call, tool_call_update, and usage_update (see
// [acp.ToSessionUpdate]'s doc for the exhaustive list). Any variant this
// renderer doesn't case on still renders as a bare marker line.
type acpUpdateWire struct {
	Update json.RawMessage `json:"update"`
}

// acpUpdateDisc decodes just the "sessionUpdate" discriminator shared by
// every update variant.
type acpUpdateDisc struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// acpContentChunkWire decodes the agent_message_chunk / agent_thought_chunk
// shape: {"sessionUpdate":...,"content":{"type":"text","text":...}}.
type acpContentChunkWire struct {
	Content json.RawMessage `json:"content"`
}

// acpToolCallWire decodes the subset of tool_call / tool_call_update fields
// this renderer shows as a one-line status marker — title, kind, status —
// deliberately not the (possibly large) raw input/output.
type acpToolCallWire struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
}

// acpUsageUpdateWire decodes the usage_update shape:
// {"sessionUpdate":"usage_update","used":...,"size":...,"cost"?:{"amount":...,"currency":...}}.
type acpUsageUpdateWire struct {
	Used uint64 `json:"used"`
	Size uint64 `json:"size"`
	Cost *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
}

// acpRenderer renders session/update notification params as a legible
// streaming transcript: the daemon-driven counterpart of internal/render's
// Human renderer, over ACP's session/update wire shape instead of the SDK's
// typed event.Event. It is intentionally small and DISTINCT from
// internal/render — a different wire contract, a different (much narrower)
// set of variants, and no reason to force one type to serve both.
type acpRenderer struct {
	w     io.Writer
	color bool
	err   error
}

// newACPRenderer returns an [acpRenderer] writing to w. When color is true,
// reasoning is wrapped in ANSI dim codes; callers should enable it only for a
// terminal sink (see colorEnabled).
func newACPRenderer(w io.Writer, color bool) *acpRenderer {
	return &acpRenderer{w: w, color: color}
}

// render decodes and writes one session/update notification's params. A
// decode failure renders as a marker line rather than aborting the stream —
// a daemon/client protocol drift should be visible, not fatal to an
// otherwise-successful turn.
func (r *acpRenderer) render(params json.RawMessage) error {
	var n acpUpdateWire
	if err := json.Unmarshal(params, &n); err != nil {
		r.marker("session/update", fmt.Sprintf("undecodable: %v", err))
		return r.err
	}
	var disc acpUpdateDisc
	if err := json.Unmarshal(n.Update, &disc); err != nil {
		r.marker("session/update", fmt.Sprintf("undecodable: %v", err))
		return r.err
	}

	switch disc.SessionUpdate {
	case "agent_message_chunk":
		r.write(r.contentText(n.Update))
	case "agent_thought_chunk":
		r.write(r.dim(r.contentText(n.Update)))
	case "tool_call":
		var tc acpToolCallWire
		_ = json.Unmarshal(n.Update, &tc)
		r.marker("tool_call", fmt.Sprintf("%s (%s)", tc.Title, tc.ToolCallID))
	case "tool_call_update":
		var tc acpToolCallWire
		_ = json.Unmarshal(n.Update, &tc)
		detail := tc.ToolCallID
		if tc.Status != "" {
			detail = fmt.Sprintf("%s → %s", tc.ToolCallID, tc.Status)
		}
		r.marker("tool_call_update", detail)
	case "usage_update":
		var u acpUsageUpdateWire
		_ = json.Unmarshal(n.Update, &u)
		detail := fmt.Sprintf("%d/%d tokens", u.Used, u.Size)
		if u.Cost != nil {
			detail = fmt.Sprintf("%s ($%.4f %s)", detail, u.Cost.Amount, u.Cost.Currency)
		}
		r.marker("usage", detail)
	default:
		r.marker(disc.SessionUpdate, "")
	}
	return r.err
}

// contentText extracts a content chunk's text, or "" if raw does not decode
// to a text content block (ACP's only other block variant, resource_link,
// never appears in a session/update — see [acp.ToSessionUpdate]).
func (r *acpRenderer) contentText(raw json.RawMessage) string {
	var chunk acpContentChunkWire
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return ""
	}
	block, err := acp.UnmarshalContentBlock(chunk.Content)
	if err != nil {
		return ""
	}
	if t, ok := block.(acp.TextContentBlock); ok {
		return t.Text
	}
	return ""
}

// marker writes a one-line "· kind  detail" marker, omitting the detail when
// empty — mirrors internal/render's Human.marker.
func (r *acpRenderer) marker(kind, detail string) {
	if detail == "" {
		r.write(fmt.Sprintf("· %s\n", kind))
		return
	}
	r.write(fmt.Sprintf("· %s  %s\n", kind, detail))
}

// dim wraps s in ANSI dim codes when color is enabled, else returns it as-is.
func (r *acpRenderer) dim(s string) string {
	if !r.color || s == "" {
		return s
	}
	return acpAnsiDim + s + acpAnsiReset
}

// write appends s to the sink, latching the first error.
func (r *acpRenderer) write(s string) {
	if r.err != nil || s == "" {
		return
	}
	_, r.err = io.WriteString(r.w, s)
}

// Package render turns a session's typed SDK event stream into output for a
// terminal client. It is deliberately dependency-light and stateless beyond a
// single write error, so the same [Renderer] drives both the live `gofer demo`
// stream and table-driven tests over recorded event sequences.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/event"
)

// Renderer consumes a session's events one at a time, in broker-delivery order,
// writing a representation of each to an underlying sink. Render returns the
// first write or encoding error encountered; once one occurs, subsequent calls
// are no-ops that return the same error.
type Renderer interface {
	// Render processes a single event, writing its representation to the sink.
	Render(e event.Event) error
}

// ANSI SGR codes used to dim reasoning output when color is enabled.
const (
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

// Human renders events as a legible streaming transcript: reasoning is dimmed
// and prefixed, assistant text is streamed raw, and lifecycle, tool, and
// permission events render as one-line markers. The turn's stop reason and
// usage are emitted as a final summary line.
type Human struct {
	w     io.Writer
	color bool
	err   error
}

// NewHuman returns a [Human] renderer writing to w. When color is true,
// reasoning is wrapped in ANSI dim codes; callers should enable it only for a
// terminal sink.
func NewHuman(w io.Writer, color bool) *Human {
	return &Human{w: w, color: color}
}

// messageCount renders n as a pluralized message count. The three compaction
// kinds all report one, and they must agree: a reader correlating a start with
// its terminal is comparing these figures by eye.
func messageCount(n int) string {
	if n == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", n)
}

// Render writes a human-readable representation of e. Message deltas stream
// inline (reasoning dimmed, text raw); every other event kind renders as a
// single line, except MessageFinished{MessageUser}, which echoes the user's
// own prompt verbatim and so may itself span multiple lines.
func (h *Human) Render(e event.Event) error {
	switch ev := e.(type) {
	case event.MessageStarted:
		if ev.MessageKind == event.MessageReasoning {
			h.write(h.dim("» "))
		}
	case event.MessageDelta:
		if ev.MessageKind == event.MessageReasoning {
			h.write(h.dim(ev.Text))
		} else {
			h.write(ev.Text)
		}
	case event.MessageFinished:
		// event.MessageUser (the user's own prompt) never deltas — see its
		// doc — so its settled MessageFinished is the only signal this
		// renderer gets; print it directly rather than the bare newline
		// every other kind gets (which closes out a stream already written
		// via MessageDelta above).
		if ev.MessageKind == event.MessageUser {
			h.write(fmt.Sprintf("you › %s\n", ev.Content))
			break
		}
		h.write("\n")
	case event.TurnFinished:
		h.write(fmt.Sprintf("· turn.finished  stop=%s  usage=%din/%dout\n",
			ev.StopReason, ev.Usage.InputTokens, ev.Usage.OutputTokens))
	case event.ToolCallStarted:
		h.marker(ev.Kind(), fmt.Sprintf("%s (%s)", ev.Name, ev.ID))
	case event.ToolCallFinished:
		h.marker(ev.Kind(), ev.ID)
	case event.PermissionRequested:
		h.marker(ev.Kind(), fmt.Sprintf("%s (%s)", ev.Tool, ev.ID))
	case event.PermissionResolved:
		h.marker(ev.Kind(), fmt.Sprintf("%s → %s", ev.ID, ev.Verdict))
	case event.SessionError:
		h.marker(ev.Kind(), ev.Err)
	case event.SessionCompactionStarted:
		// The start of a compaction, which streams nothing and can run a minute
		// or more. In a headless stream this line is the only thing standing
		// between the operator and a silent, unexplained stall.
		h.marker(ev.Kind(), fmt.Sprintf("summarizing %s…", messageCount(ev.Messages)))
	case event.SessionCompacted:
		// Compaction must never render as a bare, detail-free marker like the
		// default case below — an operator watching this stream needs to see
		// WHAT changed, not just that some lifecycle event fired.
		h.marker(ev.Kind(), fmt.Sprintf("%s replaced with a summary", messageCount(ev.MessagesCompacted)))
	case event.SessionCompactionFailed:
		// The other terminal. It must carry the reason: an operator who saw the
		// start line above needs to know the compaction did NOT land, and that
		// the session's context is therefore unchanged rather than summarized.
		h.marker(ev.Kind(), fmt.Sprintf("%s left uncompacted: %s", messageCount(ev.Messages), ev.Err))
	default:
		h.marker(e.Kind(), "")
	}
	return h.err
}

// marker writes a one-line "· kind  detail" marker, omitting the detail when
// empty. detail comes from free-form producer strings (e.g.
// [event.SessionError]'s Err) that carry no guarantee against embedded
// newlines — errors.Join, for one, joins with "\n" — so it is sanitized
// through [collapseWhitespace] first. Without that, a multiline detail would
// split the marker across terminal rows and misalign every marker after it
// in the stream, defeating the one-line contract this doc comment claims.
func (h *Human) marker(kind, detail string) {
	if detail = collapseWhitespace(detail); detail == "" {
		h.write(fmt.Sprintf("· %s\n", kind))
		return
	}
	h.write(fmt.Sprintf("· %s  %s\n", kind, detail))
}

// collapseWhitespace collapses every run of whitespace in s — including
// embedded "\n"/"\r\n" — to a single space, and trims the result. It exists
// to enforce a one-line render invariant against a caller-supplied string
// with no such guarantee of its own.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// dim wraps s in ANSI dim codes when color is enabled, else returns it as-is.
func (h *Human) dim(s string) string {
	if !h.color {
		return s
	}
	return ansiDim + s + ansiReset
}

// write appends s to the sink, latching the first error.
func (h *Human) write(s string) {
	if h.err != nil {
		return
	}
	_, h.err = io.WriteString(h.w, s)
}

// JSONL renders each event as a single JSON line — the contract's wire form,
// one event per line — using the event's own [json.Marshaler].
type JSONL struct {
	w   io.Writer
	err error
}

// NewJSONL returns a [JSONL] renderer writing newline-delimited JSON to w.
func NewJSONL(w io.Writer) *JSONL {
	return &JSONL{w: w}
}

// Render marshals e to its wire JSON and writes it followed by a newline.
func (j *JSONL) Render(e event.Event) error {
	if j.err != nil {
		return j.err
	}
	b, err := json.Marshal(e)
	if err != nil {
		j.err = fmt.Errorf("render: marshal %s: %w", e.Kind(), err)
		return j.err
	}
	_, j.err = j.w.Write(append(b, '\n'))
	return j.err
}

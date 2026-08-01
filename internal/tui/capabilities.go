package tui

// capabilities.go is the shared state and background load behind the /mcp and
// /skills command-panel tabs (mcp_view.go, skills_view.go). Both tabs render
// ONE [capability.Answer], so they share one fetch: opening either — or
// tabbing into either — dispatches at most one [App.loadCapabilitiesCmd] per
// panel, and the answer feeds both bodies.
//
// The fetch is a tea.Cmd for the same reason [App.listSessionsCmd] is: on the
// daemon path it is a round trip to another process, and resolving it inline
// would freeze the Update loop for as long as that process takes.

import (
	"context"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/capability"
)

// capabilitiesTimeout bounds the capability fetch. A package constant rather
// than a config knob, for the same reason [daemonProbeTimeout] is: it is a
// single read-only RPC on an already-open connection, no deployment would
// reasonably want to wait longer, and the cost of giving up is only that the
// panel says UNKNOWN.
const capabilitiesTimeout = 3 * time.Second

// capabilitiesState is the /mcp and /skills tabs' shared state, captured onto
// the panel at open time.
//
// It has THREE distinguishable states, and each renders differently, because
// collapsing any two of them produces a sentence that is not true:
//
//   - pending && !loaded — a fetch is in flight ("Loading…").
//   - loaded && answer.Known — a real report, which may legitimately be empty
//     ("no MCP servers configured").
//   - everything else — UNKNOWN: no capability source at all (pending is
//     false, i.e. [CommandEnv.Capabilities] is nil), or a backend that was
//     asked and could not answer.
type capabilitiesState struct {
	// pending reports that a fetch was (or will be) dispatched — i.e. the env
	// carries a capability source at all. False leaves the views on UNKNOWN
	// immediately rather than on a "Loading…" line that would never resolve.
	pending bool
	// loaded reports that a fetch has landed. It also guards the tab-in
	// re-fetch, so bouncing between the two tabs costs one round trip, not one
	// per bounce.
	loaded bool
	// answer is the landed report. Known=false is UNKNOWN, NOT "empty".
	answer capability.Answer
}

// capabilitiesLoadedMsg carries a completed capability fetch. It has no error
// field on purpose: the panel's only possible response to a failed read is to
// say UNKNOWN, which an unknown Answer already expresses, and surfacing a
// transport error the user cannot act on would talk over whatever they are
// actually reading (the same rule [modelsLoadedMsg] follows).
type capabilitiesLoadedMsg struct{ answer capability.Answer }

// loadCapabilitiesCmd fetches the backend's capability report OFF the Update
// loop. It returns nil when the env carries no capability source, so a caller
// can dispatch it unconditionally.
//
// Every failure collapses to the unknown answer — a timeout, a transport
// error, a daemon that does not implement the method. It never falls back to
// any local read: on the daemon path a local snapshot would describe THIS
// process's config while the panel claims to describe the daemon's.
func (a App) loadCapabilitiesCmd() tea.Cmd {
	probe := a.commandEnv.Capabilities
	if probe == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), capabilitiesTimeout)
		defer cancel()
		answer, err := probe(ctx)
		if err != nil {
			answer = capability.Answer{}
		}
		return capabilitiesLoadedMsg{answer: answer}
	}
}

// applyCapabilitiesLoaded folds a completed fetch into the open panel. A panel
// CLOSED while the fetch was in flight drops the result — the same rule
// [App.applySessionsListed] and [App.applyModelsLoaded] follow, and for the
// same reason: the next open re-fetches, and a stale answer must never
// resurrect a dismissed panel.
func (a App) applyCapabilitiesLoaded(msg capabilitiesLoadedMsg) App {
	if a.panel == nil {
		return a
	}
	p := *a.panel
	p.caps.loaded = true
	p.caps.answer = msg.answer
	a.panel = &p
	return a
}

// capabilityTab reports whether tab is one of the two the capability report
// feeds — the single place that pairing is spelled out, so the open-time
// dispatch (command.go's openPanel) and the tab-in dispatch
// ([App.handlePanelKey]) can never disagree about which tabs need the fetch.
func capabilityTab(tab commandPanelTab) bool {
	return tab == panelMCP || tab == panelSkills
}

// unknownCapabilityLines is the UNKNOWN body both tabs render, with subject
// naming what could not be answered. It is deliberately WORDY: the failure
// this text exists to prevent is a reader mistaking an unanswered panel for an
// empty one, and "UNKNOWN" alone next to a blank area does not prevent it.
//
// Every distinction here is carried by TEXT, never by color — the Ascii
// goldens that pin these bodies cannot see color at all (see
// internal/tui/testkit).
func unknownCapabilityLines(subject string) []string {
	return []string{
		subject + ": UNKNOWN — this backend cannot report it.",
		"This is NOT \"none configured\": nothing here has been read.",
		"An attached `gofer daemon --workers` router owns no supervisor, so",
		"no MCP manager and no skill set exist in that process to report;",
		"each session's worker owns its own. A daemon older than",
		"gofer/capabilities answers the same way.",
	}
}

// loadingCapabilityLines is the in-flight body both tabs render.
func loadingCapabilityLines(subject string) []string {
	return []string{"Loading " + subject + "…"}
}

// fitRows lays a variable-length item list between fixed head and tail lines
// inside a height budget, replacing the overflow with an explicit "+N more"
// row rather than letting [commandPanel.View] clip it silently.
//
// The overflow line is why the panel's body budget ([panelBodyRows]) does not
// grow for these tabs: an operator can configure any number of MCP servers or
// skills, so no fixed constant is large enough, and growing it would cost
// every other golden in the package a re-capture for a case that still
// overflows one server later.
//
// # Priority when even the notice does not fit
//
// On a short terminal the budget can be smaller than head+tail alone, leaving
// NEGATIVE room for items. The ranking is head > notice > tail, and the notice
// outranking the tail is the load-bearing part: the head asserts a count
// ("MCP servers: 1 connected of 6 configured"), so a reader who loses the
// aggregate figures still knows the list is incomplete, whereas a reader who
// loses the notice is looking at a header claiming six servers above nothing at
// all. An earlier revision handled only a POSITIVE budget and let the negative
// case fall through every branch, dropping the items AND the notice — exactly
// the silent clipping this function exists to prevent, at heights 7-9.
//
// The returned slice may therefore exceed height; each view's own View clips
// from the END, which is what keeps the notice on screen and spends the
// shortfall on the tail rows instead.
func fitRows(head, items, tail []string, height int) []string {
	out := make([]string, 0, len(head)+len(items)+len(tail))
	out = append(out, head...)
	room := height - len(head) - len(tail)
	if len(items) == 0 || room >= len(items) {
		// Everything fits — and an empty list must never produce a "+0 more"
		// notice, which a negative budget would otherwise reach.
		out = append(out, items...)
		return append(out, tail...)
	}
	// Overflow: one row goes to the notice, charged to the ITEMS before the
	// tail. max clamps a negative budget to "show nothing, but say so" rather
	// than leaving the case unhandled.
	shown := max(room-1, 0)
	out = append(out, items[:shown]...)
	out = append(out, "  +"+strconv.Itoa(len(items)-shown)+" more")
	return append(out, tail...)
}

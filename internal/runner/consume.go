package runner

import (
	"encoding/json"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// toolSeed is a tool call's name and input, captured from tool.call.started
// and held until the matching tool.call.finished arrives with its result.
type toolSeed struct {
	Name  string
	Input json.RawMessage
}

// turnAcc accumulates one model-call iteration's settled output across
// events. It cannot flush strictly on turn.finished: for a tool-use
// iteration the loop publishes turn.finished *before* it executes the
// requested tools (execution, and each tool.call.finished, happens
// afterward, in the same iteration, before the next turn.started). A turn is
// only fully settled — safe to journal — once turn.finished has been seen
// *and* every tool call it announced has a matching result.
type turnAcc struct {
	text      strings.Builder
	reasoning strings.Builder
	pending   map[string]toolSeed
	done      []session.ToolCallRecord
	usage     provider.Usage
	settled   bool // turn.finished observed for the iteration in progress
}

func newTurnAcc() *turnAcc {
	return &turnAcc{pending: make(map[string]toolSeed)}
}

// ready reports whether the accumulated iteration is fully settled: its
// turn.finished has arrived and no tool call it started is still pending a
// result.
func (a *turnAcc) ready() bool {
	return a.settled && len(a.pending) == 0
}

// reset clears the accumulator for the next iteration.
func (a *turnAcc) reset() {
	a.text.Reset()
	a.reasoning.Reset()
	for id := range a.pending {
		delete(a.pending, id)
	}
	a.done = nil
	a.usage = provider.Usage{}
	a.settled = false
}

// consume drains sub until the broker closes it, journaling each iteration's
// settled output as soon as it is ready (see turnAcc). It runs on its own
// goroutine for the lifetime of the Runner; Close waits for it to finish
// draining before closing the journal, so a killed run's already-settled
// prefix is guaranteed durable once Close returns.
func (r *Runner) consume(sub *event.Subscription) {
	defer close(r.journalDone)

	acc := newTurnAcc()
	for e := range sub.C {
		switch ev := e.(type) {
		case event.MessageFinished:
			switch ev.MessageKind {
			case event.MessageText:
				acc.text.WriteString(ev.Content)
			case event.MessageReasoning:
				acc.reasoning.WriteString(ev.Content)
			}

		case event.ToolCallStarted:
			acc.pending[ev.ID] = toolSeed{Name: ev.Name, Input: ev.Input}

		case event.ToolCallFinished:
			seed := acc.pending[ev.ID]
			delete(acc.pending, ev.ID)
			acc.done = append(acc.done, session.ToolCallRecord{
				ID:     ev.ID,
				Name:   seed.Name,
				Input:  seed.Input,
				Result: ev.Result,
				// IsError: tool.call.finished carries no error flag (an SDK
				// contract gap at M1 — see docs/M1-PROOF.md) — default false.
			})
			if acc.ready() {
				r.flushTurn(acc)
				acc.reset()
			}

		case event.TurnFinished:
			acc.usage = ev.Usage
			acc.settled = true
			if acc.ready() {
				r.flushTurn(acc)
				acc.reset()
			}
		}
	}
}

// flushTurn appends the accumulator's settled output to the journal: an
// assistant message entry when the iteration produced text or reasoning, and
// a tool-round entry when it produced tool calls. Either, both, or neither
// may apply to a given iteration.
func (r *Runner) flushTurn(acc *turnAcc) {
	text := acc.text.String()
	reasoning := acc.reasoning.String()
	if text != "" || reasoning != "" {
		opts := []session.EntryOpt{
			session.WithEntryModel(r.model),
			session.WithEntryUsage(acc.usage),
		}
		if reasoning != "" {
			opts = append(opts, session.WithReasoning(reasoning))
		}
		if _, err := r.journal.Append(session.NewMessageEntry("assistant", text, opts...)); err != nil {
			r.setJournalWriteErr(err)
		}
	}

	if len(acc.done) > 0 {
		entry := session.NewToolRoundEntry(acc.done, session.WithEntryModel(r.model))
		if _, err := r.journal.Append(entry); err != nil {
			r.setJournalWriteErr(err)
		}
	}
}

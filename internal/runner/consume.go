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

// turnAcc accumulates one model-call iteration's settled output across events,
// then journals it in two independently-flushed parts:
//
//   - The assistant MESSAGE (text/reasoning + usage) flushes at turn.finished,
//     which is when usage arrives. This is deliberately decoupled from tool
//     execution: the loop emits turn.finished for a tool-use iteration *before*
//     it runs the tools, so gating the message flush on tool results would lose
//     already-settled text if the run is killed while a tool call is still
//     streaming (started, no result) — the tool.call.finished that would drain
//     `pending` never arrives.
//   - The tool ROUND flushes once every announced call has a result (pending
//     drained), which for a tool-use turn happens after turn.finished.
//
// Tools only run on a StopToolUse turn; on any other stop reason (end_turn,
// cancelled, error, …) the loop returns without executing pending calls, so
// those orphaned started-but-unexecuted calls are dropped — never left to wedge
// the accumulator or to emit a dangling tool_use with no matching result.
type turnAcc struct {
	text       strings.Builder
	reasoning  strings.Builder
	usage      provider.Usage
	msgFlushed bool // assistant message entry already written for this turn
	pending    map[string]toolSeed
	done       []session.ToolCallRecord
	finished   bool // turn.finished observed for the iteration in progress
}

func newTurnAcc() *turnAcc {
	return &turnAcc{pending: make(map[string]toolSeed)}
}

// reset clears the accumulator for the next iteration.
func (a *turnAcc) reset() {
	a.text.Reset()
	a.reasoning.Reset()
	a.usage = provider.Usage{}
	a.msgFlushed = false
	for id := range a.pending {
		delete(a.pending, id)
	}
	a.done = nil
	a.finished = false
}

// consume drains sub until the broker closes it, journaling each iteration's
// settled output as it settles (see turnAcc). It runs on its own goroutine for
// the lifetime of the Runner; Close waits for it to finish draining before
// closing the journal, so a killed run's already-settled prefix is guaranteed
// durable once Close returns.
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
			r.maybeFlushRound(acc)

		case event.TurnFinished:
			acc.usage = ev.Usage
			acc.finished = true
			// The assistant message has settled (usage is available); flush it
			// now, independent of any tool round, so a kill during tool-call
			// streaming cannot strand already-settled text/reasoning.
			r.flushMessage(acc)
			if ev.StopReason != string(provider.StopToolUse) {
				// Tools run only on a tool_use stop; on any other stop reason the
				// loop returns without executing them, so no tool.call.finished
				// will arrive. Drop the orphaned started-but-unexecuted calls
				// rather than wedge the accumulator forever.
				for id := range acc.pending {
					delete(acc.pending, id)
				}
			}
			r.maybeFlushRound(acc)
		}
	}

	// Belt-and-suspenders: if the stream tore down with settled text that never
	// saw a turn.finished (an out-of-band teardown), persist it rather than
	// silently dropping it. No-op after a normal reset or an already-flushed
	// message.
	r.flushMessage(acc)
}

// flushMessage appends the assistant message entry (settled text/reasoning +
// usage) at most once per turn. It no-ops when nothing textual settled or the
// entry was already written.
func (r *Runner) flushMessage(acc *turnAcc) {
	if acc.msgFlushed {
		return
	}
	text := acc.text.String()
	reasoning := acc.reasoning.String()
	if text == "" && reasoning == "" {
		return
	}
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
	acc.msgFlushed = true
}

// maybeFlushRound appends the tool-round entry once the turn has finished and
// every announced call has a result (pending drained), then resets the
// accumulator for the next iteration. It no-ops while calls are still pending —
// a tool-use turn between its turn.finished and its tool results — and while the
// turn has not finished.
func (r *Runner) maybeFlushRound(acc *turnAcc) {
	if !acc.finished || len(acc.pending) > 0 {
		return
	}
	if len(acc.done) > 0 {
		entry := session.NewToolRoundEntry(acc.done, session.WithEntryModel(r.model))
		if _, err := r.journal.Append(entry); err != nil {
			r.setJournalWriteErr(err)
		}
	}
	acc.reset()
}

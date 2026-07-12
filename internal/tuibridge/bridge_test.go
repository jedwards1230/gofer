package tuibridge

import (
	"reflect"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// TestToTUICopiesRenderedFields verifies every field the TUI renders is carried
// across, and the operational extras are simply not present on the TUI type.
func TestToTUICopiesRenderedFields(t *testing.T) {
	created := time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)
	updated := created.Add(3 * time.Minute)
	in := supervisor.SessionInfo{
		ID:        "0192a1b2-0000-7000-8000-00000000000a",
		Title:     "wire the bridge",
		Summary:   "mapping supervisor rows to the TUI",
		Status:    supervisor.StatusNeedsInput,
		Model:     "fable-5",
		Cost:      provider.Cost{USD: 0.2417},
		Usage:     provider.Usage{InputTokens: 42, OutputTokens: 19},
		Pending:   1,
		Artifacts: 3,
		Created:   created,
		Updated:   updated,
		// operational extras the TUI ignores:
		Project: "gofer", JournalPath: "/x.jsonl", Queued: 2, Live: true,
	}

	got := toTUI(in)
	want := tui.SessionInfo{
		ID:        in.ID,
		Title:     in.Title,
		Summary:   in.Summary,
		Status:    tui.StatusNeedsInput,
		Model:     in.Model,
		Cost:      in.Cost,
		Usage:     in.Usage,
		Pending:   in.Pending,
		Artifacts: in.Artifacts,
		Created:   in.Created,
		Updated:   in.Updated,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toTUI mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestStatusEnumsShareOrdinals pins the assumption the status cast relies on:
// the supervisor and TUI SessionStatus enums must share ordinals, or a plain
// int cast would silently mislabel roster rows. If either enum is reordered or
// gains a value out of step, this fails loudly.
func TestStatusEnumsShareOrdinals(t *testing.T) {
	cases := []struct {
		in   supervisor.SessionStatus
		want tui.SessionStatus
	}{
		{supervisor.StatusWorking, tui.StatusWorking},
		{supervisor.StatusNeedsInput, tui.StatusNeedsInput},
		{supervisor.StatusFinished, tui.StatusFinished},
	}
	for _, c := range cases {
		if got := toTUI(supervisor.SessionInfo{Status: c.in}).Status; got != c.want {
			t.Errorf("supervisor status %v mapped to tui %v, want %v", c.in, got, c.want)
		}
	}
}

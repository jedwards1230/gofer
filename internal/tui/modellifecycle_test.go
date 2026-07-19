package tui

// modellifecycle_test.go covers the /model picker's BACKGROUND CATALOG LOAD —
// the lifecycle that spans openPanel (command.go), App.discoverModelsCmd /
// App.applyModelsLoaded (panel.go) and modelPickerView.withCatalog
// (modelpicker.go), rather than any one of them in isolation.
//
// The property under test throughout is that a live listing is an UPGRADE and
// never a regression: the compiled-in floor is on screen from the first frame,
// opening the panel never waits for the vendor, a completed load swaps the list
// in underneath the user without moving what they had selected, and every
// failure mode — an erroring closure, a nil closure, a result arriving after the
// panel closed — leaves a usable list rather than an empty picker.
//
// White-box (package tui) because the whole lifecycle is unexported: the
// modelsLoadedMsg, the command that produces it, and the view's cached list are
// all internal. No test here performs IO — CommandEnv.Models is a closure a test
// supplies, so the real transport is exercised at its production call site in
// cmd/gofer instead (TestProductionCommandEnvPerformsDiscovery).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/modelcatalog"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// codexFloorIDs is the compiled-in OpenAI-OAuth floor, in order — what the
// picker must show before any load completes and must still show after any
// load fails.
var codexFloorIDs = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}

// liveCatalog is a listing deliberately unlike the floor: a different order, an
// id the floor does not carry, and one floor id dropped. An assertion that
// passes against this cannot also pass against the floor, which is what makes
// "the swap happened" and "the swap did not happen" distinguishable.
func liveCatalog() []modelcatalog.Model {
	return []modelcatalog.Model{
		{ID: "gpt-5.7-nova", Provider: "openai", Label: "GPT-5.7 Nova", ContextWindow: 400000},
		{ID: "gpt-5.6-terra", Provider: "openai", Label: "GPT-5.6 Terra (live)", ContextWindow: 272000},
		{ID: "gpt-5.3-codex-spark", Provider: "openai", Label: "GPT-5.3 Codex Spark"},
	}
}

// oauthModelEnv is a CommandEnv reporting one OpenAI OAuth credential — the
// issue #157 user — with models as its load closure.
func oauthModelEnv(models func(context.Context, string) ([]modelcatalog.Model, error)) CommandEnv {
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) {
		return []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}, nil
	}
	env.Models = models
	return env
}

// openModelPanel drives the REAL open path (command.go's openPanel for the
// Model tab), returning the resulting App and the background command it
// dispatched. Going through openPanel rather than constructing a panel by hand
// is the point: the dispatch of the load is part of what is under test.
func openModelPanel(t *testing.T, env CommandEnv) (App, tea.Cmd) {
	t.Helper()
	a := NewApp(theme.Test(), newInternalFakeSup(nil), GoldenMeta(), env)
	return openPanel(panelModel)(a, nil)
}

// pickerIDs flattens the picker's cached list to ids, in render order.
func pickerIDs(v modelPickerView) []string {
	out := make([]string, 0, len(v.models))
	for _, m := range v.models {
		out = append(out, m.ID)
	}
	return out
}

// TestModelPickerRendersFloorOnFirstFrame is the "usable immediately" half of
// the design: the panel is fully populated the instant it opens, from the
// compiled-in floor, with no load having completed (or even started returning).
// A picker that opened empty and filled in later would be a regression even if
// the fill were fast — the user asked to pick a model, and there is a correct
// offline answer available at zero cost.
func TestModelPickerRendersFloorOnFirstFrame(t *testing.T) {
	// A load that never returns, so nothing below can be explained by it
	// having completed.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	env := oauthModelEnv(func(ctx context.Context, _ string) ([]modelcatalog.Model, error) {
		select {
		case <-blocked:
		case <-ctx.Done():
		}
		return nil, errors.New("never returns")
	})

	a, _ := openModelPanel(t, env)
	if a.panel == nil {
		t.Fatal("openPanel left no panel open")
	}
	v := a.panel.model

	if got := pickerIDs(v); !slices.Equal(got, codexFloorIDs) {
		t.Errorf("first-frame list = %v, want the compiled-in floor %v", got, codexFloorIDs)
	}
	if v.live {
		t.Error("live = true with no load completed; it must record a real load, not the seeded floor")
	}
	// And it is genuinely on screen, not merely in the struct.
	frame := testkit.Render(v, testkit.Width, testkit.Height)
	for _, id := range codexFloorIDs {
		if !strings.Contains(frame, id) {
			t.Errorf("first frame does not render floor model %q:\n%s", id, frame)
		}
	}
}

// TestOpeningModelPickerDoesNotBlockOnTheLoad locks the other half: the vendor
// round trip is bounded at modelcatalog.DefaultDiscoveryTimeout (3s), which is
// an eternity in a key handler. Opening must return immediately and let the
// listing land later.
//
// The load closure here blocks until the test releases it, so a synchronous
// call site would hang rather than merely be slow — a "fast enough" threshold
// cannot pass by luck.
func TestOpeningModelPickerDoesNotBlockOnTheLoad(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	env := oauthModelEnv(func(ctx context.Context, _ string) ([]modelcatalog.Model, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return liveCatalog(), nil
	})

	done := make(chan App, 1)
	go func() {
		a, cmd := openModelPanel(t, env)
		if cmd == nil {
			t.Error("openPanel dispatched no background load command")
		}
		done <- a
	}()

	select {
	case a := <-done:
		// Opening returned while the load is still blocked, which is the
		// property. The panel is populated from the floor already.
		if a.panel == nil || len(a.panel.model.models) == 0 {
			t.Fatal("opened with an empty picker")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening /model blocked on the catalog load; it must run off the Update loop")
	}

	// The closure must not have been called on the opening goroutine at all.
	select {
	case <-entered:
		t.Error("the load closure ran during openPanel; the load must be deferred to the returned tea.Cmd")
	default:
	}
}

// TestCompletedLoadReplacesTheList proves the whole feature actually works
// end to end at this layer: running the dispatched command produces a
// modelsLoadedMsg, and folding it in swaps the floor for the live listing and
// records that it is live.
func TestCompletedLoadReplacesTheList(t *testing.T) {
	var gotProvider string
	env := oauthModelEnv(func(_ context.Context, providerID string) ([]modelcatalog.Model, error) {
		gotProvider = providerID
		return liveCatalog(), nil
	})

	a, cmd := openModelPanel(t, env)
	if cmd == nil {
		t.Fatal("openPanel dispatched no background load command")
	}

	msg, ok := cmd().(modelsLoadedMsg)
	if !ok {
		t.Fatalf("command produced %T, want modelsLoadedMsg", cmd())
	}
	if gotProvider != "openai" {
		t.Errorf("loaded provider = %q, want the authenticated provider %q", gotProvider, "openai")
	}

	a = a.applyModelsLoaded(msg)
	v := a.panel.model

	want := []string{"gpt-5.7-nova", "gpt-5.6-terra", "gpt-5.3-codex-spark"}
	if got := pickerIDs(v); !slices.Equal(got, want) {
		t.Errorf("list after load = %v, want the live listing %v", got, want)
	}
	if !v.live {
		t.Error("live = false after a completed load")
	}
	// The live label wins over the compiled-in one — the point of asking.
	frame := testkit.Render(v, testkit.Width, testkit.Height)
	if !strings.Contains(frame, "GPT-5.6 Terra (live)") {
		t.Errorf("rendered frame does not carry the live display name:\n%s", frame)
	}
}

// TestFailedLoadLeavesTheFloorInPlace is the hard requirement: the picker is
// NEVER empty. Every way the load can come back with nothing must leave the
// floor exactly as it was — a stale-but-usable list beats a blank panel, and a
// user on a flaky network still gets to pick a model.
func TestFailedLoadLeavesTheFloorInPlace(t *testing.T) {
	tests := []struct {
		name  string
		load  func(context.Context, string) ([]modelcatalog.Model, error)
		nilFn bool
	}{
		{
			name: "closure errors (broken auth.json)",
			load: func(context.Context, string) ([]modelcatalog.Model, error) {
				return nil, errors.New("read openai credential: unexpected end of JSON input")
			},
		},
		{
			name: "closure returns nothing",
			load: func(context.Context, string) ([]modelcatalog.Model, error) { return nil, nil },
		},
		{
			name: "context deadline exceeded (timed out)",
			load: func(context.Context, string) ([]modelcatalog.Model, error) {
				return nil, context.DeadlineExceeded
			},
		},
		{
			name:  "no Models closure at all (zero CommandEnv)",
			nilFn: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := oauthModelEnv(tt.load)
			if tt.nilFn {
				env.Models = nil
			}

			a, cmd := openModelPanel(t, env)
			if cmd != nil {
				a = a.applyModelsLoaded(cmd().(modelsLoadedMsg))
			}
			v := a.panel.model

			if len(v.models) == 0 {
				t.Fatal("the picker is EMPTY after a failed load; the floor must never be replaced by nothing")
			}
			if got := pickerIDs(v); !slices.Equal(got, codexFloorIDs) {
				t.Errorf("list = %v, want the untouched floor %v", got, codexFloorIDs)
			}
			if v.live {
				t.Error("live = true after a load that produced nothing")
			}
		})
	}
}

// TestLateLoadAfterPanelClosedIsDropped covers the stale-arrival case: the user
// dismissed the panel while the request was in flight. The result must be
// discarded — never applied to a closed panel, and above all never used to
// re-open one the user deliberately closed. It must also not panic on the nil
// panel, which is the shape a naive implementation gets wrong.
func TestLateLoadAfterPanelClosedIsDropped(t *testing.T) {
	env := oauthModelEnv(func(context.Context, string) ([]modelcatalog.Model, error) {
		return liveCatalog(), nil
	})
	a, cmd := openModelPanel(t, env)
	msg := cmd().(modelsLoadedMsg)

	// The user pressed Esc before the listing landed.
	a.panel = nil

	a = a.applyModelsLoaded(msg)
	if a.panel != nil {
		t.Error("a late catalog load re-opened a panel the user had closed")
	}
}

// TestLoadPreservesUserStateAcrossTheSwap covers the other late-arrival case:
// the panel is still open, but the user has already navigated and typed. The
// upgrade must happen UNDERNEATH them — a background list refresh that moved
// the highlight or ate a half-typed id would be a worse experience than never
// refreshing at all.
//
// The highlight is preserved by MODEL ID, not by index: the live listing is
// ordered differently from the floor, so keeping the index would silently point
// at a different model. The subtests pin exactly that distinction.
func TestLoadPreservesUserStateAcrossTheSwap(t *testing.T) {
	load := func(context.Context, string) ([]modelcatalog.Model, error) { return liveCatalog(), nil }

	t.Run("highlighted model survives a reordering", func(t *testing.T) {
		a, cmd := openModelPanel(t, oauthModelEnv(load))
		msg := cmd().(modelsLoadedMsg)

		// Highlight gpt-5.3-codex-spark: floor index 4, live index 2. An
		// index-preserving implementation would land out of range; a
		// highlight-dropping one would land on nothing.
		v := a.panel.model
		v.cursor = slices.Index(codexFloorIDs, "gpt-5.3-codex-spark")
		v.entry = "gpt-5.6-l"
		a.panel.model = v

		a = a.applyModelsLoaded(msg)
		got := a.panel.model

		rows := got.rows()
		if got.cursor < 0 || got.cursor >= len(rows) {
			t.Fatalf("cursor = %d after the swap, want the row for gpt-5.3-codex-spark (rows: %d)", got.cursor, len(rows))
		}
		if id := rows[got.cursor].id; id != "gpt-5.3-codex-spark" {
			t.Errorf("highlight moved to %q; it must stay on the model the user selected", id)
		}
		if got.entry != "gpt-5.6-l" {
			t.Errorf("entry = %q, want the user's typed text preserved verbatim", got.entry)
		}
	})

	t.Run("highlighted model absent from the live listing drops the highlight", func(t *testing.T) {
		a, cmd := openModelPanel(t, oauthModelEnv(load))
		msg := cmd().(modelsLoadedMsg)

		// gpt-5.6-sol is in the floor and NOT in liveCatalog: the model
		// genuinely went away, so the highlight has nothing to point at.
		v := a.panel.model
		v.cursor = slices.Index(codexFloorIDs, "gpt-5.6-sol")
		a.panel.model = v

		a = a.applyModelsLoaded(msg)
		if got := a.panel.model.cursor; got != -1 {
			t.Errorf("cursor = %d, want -1 — a retired model must not slide the highlight onto a neighbor", got)
		}
	})

	t.Run("untouched picker keeps no highlight", func(t *testing.T) {
		a, cmd := openModelPanel(t, oauthModelEnv(load))
		msg := cmd().(modelsLoadedMsg)
		a = a.applyModelsLoaded(msg)
		if got := a.panel.model.cursor; got != -1 {
			t.Errorf("cursor = %d, want -1 — a load must not select a row the user never highlighted", got)
		}
	})
}

// TestTypedUnlistedIDStillCommitsAfterLiveLoad guards the escape hatch against
// the catalog becoming an admission gate. A live listing is fresher than the
// compiled-in floor but still only "what the vendor chose to advertise" — it
// must not narrow what the user is allowed to name. Typing a routable id the
// live listing does not carry still commits.
func TestTypedUnlistedIDStillCommitsAfterLiveLoad(t *testing.T) {
	env := oauthModelEnv(func(context.Context, string) ([]modelcatalog.Model, error) {
		return liveCatalog(), nil
	})
	a, cmd := openModelPanel(t, env)
	a = a.applyModelsLoaded(cmd().(modelsLoadedMsg))

	v := a.panel.model
	if !v.live {
		t.Fatal("precondition: the live listing was not applied")
	}
	// Not in liveCatalog and not in the floor either.
	const unlisted = "gpt-5.6-luna"
	if slices.Contains(pickerIDs(v), unlisted) {
		t.Fatalf("precondition: %q must be absent from the live listing", unlisted)
	}

	v = typeModel(v, unlisted)
	if got := v.selectedModel(); got != unlisted {
		t.Errorf("selectedModel() = %q, want the typed id %q — the catalog must not gate what can be named", got, unlisted)
	}
}

// TestDiscoverModelsCmdSkipsWorkItCannotDo covers the two "nothing to fetch"
// shapes. Both must yield a nil command rather than one that runs and produces
// an empty result, so a caller can dispatch unconditionally and no request is
// issued for a user who has not signed in.
func TestDiscoverModelsCmdSkipsWorkItCannotDo(t *testing.T) {
	live := func(context.Context, string) ([]modelcatalog.Model, error) { return liveCatalog(), nil }

	t.Run("no Models closure", func(t *testing.T) {
		env := oauthModelEnv(nil)
		a := NewApp(theme.Test(), newInternalFakeSup(nil), GoldenMeta(), env)
		if cmd := a.discoverModelsCmd(); cmd != nil {
			t.Error("dispatched a load with no Models closure to run")
		}
	})

	t.Run("no authenticated provider", func(t *testing.T) {
		env := oauthModelEnv(live)
		env.Auth = func() ([]ProviderAuth, error) { return nil, nil }
		a := NewApp(theme.Test(), newInternalFakeSup(nil), GoldenMeta(), env)
		if cmd := a.discoverModelsCmd(); cmd != nil {
			t.Error("dispatched a load with no authenticated provider to load for")
		}
	})
}

// TestOpeningOtherPanelTabsIssuesNoLoad pins the cost boundary: only /model
// pays for a listing. Opening /status or /config must not issue a vendor
// request the user never asked for.
func TestOpeningOtherPanelTabsIssuesNoLoad(t *testing.T) {
	for _, tab := range []struct {
		name string
		tab  commandPanelTab
	}{{"status", panelStatus}, {"config", panelConfig}} {
		t.Run(tab.name, func(t *testing.T) {
			called := false
			env := oauthModelEnv(func(context.Context, string) ([]modelcatalog.Model, error) {
				called = true
				return liveCatalog(), nil
			})
			a := NewApp(theme.Test(), newInternalFakeSup(nil), GoldenMeta(), env)
			_, cmd := openPanel(tab.tab)(a, nil)
			if cmd != nil {
				cmd()
			}
			if called {
				t.Errorf("opening /%s issued a model listing request", tab.name)
			}
		})
	}
}

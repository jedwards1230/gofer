package tui

// keymap_test.go guards the key table (keymap.go). Its global half is LIVE —
// [App.handleKey] dispatches through it — so the tests that matter most are
// the ones that would catch a new binding colliding with an existing one, and
// the ones that prove the table is the thing actually dispatching rather than
// a parallel document.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// countingSaveEnv returns a CommandEnv whose SaveConfig only counts calls —
// enough to observe that a keymap action reached the real commit path without
// re-testing what /yolo writes (yolo_test.go owns that).
func countingSaveEnv(n *int) CommandEnv {
	env := GoldenCommandEnv()
	env.SaveConfig = func(config.Config) error {
		*n++
		return nil
	}
	return env
}

// TestGlobalBindingsAreLive is the whole justification for the table's
// existence: /help renders what dispatch runs. A row added for documentation
// only would be invisible drift, so globalKeymap rows must all carry both a
// matcher and an action.
func TestGlobalBindingsAreLive(t *testing.T) {
	rows := globalKeymap()
	if len(rows) == 0 {
		t.Fatal("globalKeymap is empty — this test would assert nothing")
	}
	for _, b := range rows {
		if !b.live() {
			t.Errorf("global binding %q (%s) has no matcher/action — a global row must be dispatched from the table, "+
				"not declared here and handled in a screen's switch", b.Keys, b.Desc)
		}
		if b.Scope != scopeGlobal {
			t.Errorf("binding %q is in globalKeymap but scoped %v", b.Keys, b.Scope)
		}
	}
}

// keyProbes are the key presses the collision check sweeps. It covers every
// binding this package claims anywhere — the global rows, the readline keymap
// (input_keymap.go), and each screen's navigation contract — so a NEW global
// binding that shadows any of them fails here rather than in a bug report about
// a key that stopped working.
func keyProbes() []struct {
	name string
	key  tea.Key
} {
	ctrlLetters := []rune{'a', 'c', 'd', 'e', 'k', 'r', 'u', 'w', 'x'}
	probes := []struct {
		name string
		key  tea.Key
	}{
		{"left", tea.Key{Code: tea.KeyLeft}},
		{"right", tea.Key{Code: tea.KeyRight}},
		{"up", tea.Key{Code: tea.KeyUp}},
		{"down", tea.Key{Code: tea.KeyDown}},
		{"home", tea.Key{Code: tea.KeyHome}},
		{"end", tea.Key{Code: tea.KeyEnd}},
		{"enter", tea.Key{Code: tea.KeyEnter}},
		{"esc", tea.Key{Code: tea.KeyEscape}},
		{"tab", tea.Key{Code: tea.KeyTab}},
		{"space", tea.Key{Code: tea.KeySpace, Text: " "}},
		{"backspace", tea.Key{Code: tea.KeyBackspace}},
		{"delete", tea.Key{Code: tea.KeyDelete}},
		{"pgup", tea.Key{Code: tea.KeyPgUp}},
		{"pgdown", tea.Key{Code: tea.KeyPgDown}},
		{"alt+left", tea.Key{Code: tea.KeyLeft, Mod: tea.ModAlt}},
		{"alt+right", tea.Key{Code: tea.KeyRight, Mod: tea.ModAlt}},
		{"alt+backspace", tea.Key{Code: tea.KeyBackspace, Mod: tea.ModAlt}},
		{"a (approval allow / peek text)", tea.Key{Code: 'a', Text: "a"}},
		{"d (approval deny)", tea.Key{Code: 'd', Text: "d"}},
		{"n (approval deny)", tea.Key{Code: 'n', Text: "n"}},
		{"r (approval remember)", tea.Key{Code: 'r', Text: "r"}},
		{"y (approval allow)", tea.Key{Code: 'y', Text: "y"}},
		{"? (help)", tea.Key{Code: '?', Text: "?"}},
	}
	for _, r := range ctrlLetters {
		probes = append(probes, struct {
			name string
			key  tea.Key
		}{"ctrl+" + string(r), tea.Key{Code: r, Mod: tea.ModCtrl}})
	}
	return probes
}

// TestGlobalBindingsDoNotCollide sweeps every key this package binds anywhere
// against the global table. A global binding steals its key from EVERY screen,
// so a collision is not a preference question — it silently removes a working
// binding. ctrl+c is expected to match (it IS the global quit); every other
// probe must be claimed by at most one global row, and none of the
// screen/readline probes may be claimed at all.
func TestGlobalBindingsDoNotCollide(t *testing.T) {
	expectedGlobals := map[string]string{
		"ctrl+c": "ctrl+c",
		"ctrl+y": "ctrl+y",
		"ctrl+r": "ctrl+r",
	}
	for _, probe := range keyProbes() {
		var matched []string
		for _, b := range globalKeymap() {
			if b.match(probe.key) {
				matched = append(matched, b.Keys)
			}
		}
		if len(matched) > 1 {
			t.Errorf("%s is claimed by %d global bindings (%v) — two rows cannot own one key", probe.name, len(matched), matched)
			continue
		}
		want, isGlobal := expectedGlobals[probe.name]
		switch {
		case isGlobal && len(matched) == 0:
			t.Errorf("%s should be the global %q but no global row matched it", probe.name, want)
		case !isGlobal && len(matched) == 1:
			t.Errorf("global binding %q claims %s, which a screen or the input keymap already binds — "+
				"a global steals its key from every screen", matched[0], probe.name)
		}
	}
}

// TestDispatchGlobalKeyRunsTheTable proves the dispatcher and the table are the
// same thing: ctrl+y routed through dispatchGlobalKey performs the toggle, and
// an unbound key is reported as unhandled so the screen handlers still see it.
func TestDispatchGlobalKeyRunsTheTable(t *testing.T) {
	var saved int
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), countingSaveEnv(&saved))

	next, _, handled := dispatchGlobalKey(a, tea.Key{Code: 'y', Mod: tea.ModCtrl})
	if !handled {
		t.Fatal("ctrl+y was not handled by the global table")
	}
	if saved != 1 {
		t.Fatalf("ctrl+y produced %d config writes, want 1 — the table's action is not wired to the toggle", saved)
	}
	if app, ok := next.(App); !ok || app.status == "" {
		t.Fatalf("ctrl+y left no status note (%T)", next)
	}

	if _, _, handled := dispatchGlobalKey(a, tea.Key{Code: 'q', Text: "q"}); handled {
		t.Error("an unbound key was reported as handled; the per-screen handlers would never see it")
	}
}

// TestScreenKeymapRowsAreDocumented is the shape check on the descriptive half.
// It cannot verify the bindings against the screens' inline switches — see
// keymap.go's doc for why that is a refactor rather than a test — but it does
// keep an empty or half-filled row out of the rendered table.
func TestScreenKeymapRowsAreDocumented(t *testing.T) {
	for _, b := range screenKeymap() {
		if b.Keys == "" || b.Desc == "" {
			t.Errorf("keymap row %+v is missing its keys or description", b)
		}
		if b.Scope == scopeGlobal {
			t.Errorf("binding %q is scoped global but declared in screenKeymap; move it to globalKeymap (with a matcher and action)", b.Keys)
		}
		if b.live() {
			t.Errorf("binding %q carries a matcher/action but is not in globalKeymap, so nothing dispatches it", b.Keys)
		}
	}
}

// TestEveryScopeIsRendered guards the one silent-drop failure mode in the
// rendering path: a scope used by a row but missing from keyScopeOrder renders
// nowhere at all.
func TestEveryScopeIsRendered(t *testing.T) {
	ordered := map[keyScope]bool{}
	for _, s := range keyScopeOrder {
		ordered[s] = true
	}
	for _, b := range keymap() {
		if !ordered[b.Scope] {
			t.Errorf("binding %q is scoped %v, which keyScopeOrder omits — it renders nowhere in /help", b.Keys, b.Scope)
		}
	}
}

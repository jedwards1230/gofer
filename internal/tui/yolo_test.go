package tui_test

// yolo_test.go covers the /yolo guardrail toggle (yolo.go): what it persists,
// what it says, what color it says it in, and that the ctrl+y key and the
// slash command are genuinely the same action rather than two implementations
// that agree today.
//
// Everything is driven through App's exported Update/View surface with the
// shared app_test.go helpers (press/type_/content) and command_test.go's
// dispatchSlash — the same way the /model select tests exercise their own
// commit path.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

const (
	yoloOnNote  = "Guardrails OFF (yolo) for NEW sessions; running sessions keep theirs."
	yoloOffNote = "Guardrails ON (ask) for new sessions; running sessions keep theirs."
)

// yoloEnv returns a CommandEnv whose config.json is a single in-memory value:
// Config reads it, SaveConfig writes it AND appends to saved, so a test can
// both assert what was written and observe the toggle reading its own previous
// write back (which is what makes the second /yolo flip the other way).
func yoloEnv(cfg *config.Config, saved *[]config.Config) tui.CommandEnv {
	env := tui.GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return *cfg, nil }
	env.SaveConfig = func(c config.Config) error {
		*cfg = c
		*saved = append(*saved, c)
		return nil
	}
	return env
}

// newYoloApp builds a sized App over the yolo env, with the first roster fetch
// resolved — newModelSelectApp's construction, parameterized on the env.
func newYoloApp(t *testing.T, env tui.CommandEnv) tea.Model {
	t.Helper()
	return newModelSelectAppWithTheme(t, theme.Test(), newFakeSup(tui.GoldenRoster()), env)
}

// TestYoloTogglesBothDirections is the core behavior: bare /yolo flips the
// persisted mode, and flips it back.
func TestYoloTogglesBothDirections(t *testing.T) {
	var cfg config.Config
	var saved []config.Config
	m := newYoloApp(t, yoloEnv(&cfg, &saved))

	m = dispatchSlash(t, m, "/yolo")
	if len(saved) != 1 || saved[0].Session.Mode() != config.PermissionModeYolo {
		t.Fatalf("after the first /yolo, saved = %+v; want one entry in yolo", saved)
	}
	if got := content(m); !strings.Contains(got, yoloOnNote) {
		t.Fatalf("expected the guardrails-off note, got:\n%s", got)
	}

	m = dispatchSlash(t, m, "/yolo")
	if len(saved) != 2 || saved[1].Session.Mode() != config.PermissionModeAsk {
		t.Fatalf("after the second /yolo, saved = %+v; want a second entry back in ask", saved)
	}
	if got := content(m); !strings.Contains(got, yoloOffNote) {
		t.Fatalf("expected the guardrails-on note, got:\n%s", got)
	}
}

// TestYoloExplicitOnOffIsIdempotent covers the stated forms: `on`/`off` set the
// posture outright, so repeating one does not flip it back the way a bare
// toggle would.
func TestYoloExplicitOnOffIsIdempotent(t *testing.T) {
	var cfg config.Config
	var saved []config.Config
	m := newYoloApp(t, yoloEnv(&cfg, &saved))

	m = dispatchSlash(t, m, "/yolo on")
	m = dispatchSlash(t, m, "/yolo on")
	if len(saved) != 2 {
		t.Fatalf("saved = %+v; want two writes", saved)
	}
	for i, c := range saved {
		if c.Session.Mode() != config.PermissionModeYolo {
			t.Fatalf("write %d = %q; `/yolo on` must be idempotent, not a toggle", i, c.Session.Mode())
		}
	}

	m = dispatchSlash(t, m, "/yolo off")
	m = dispatchSlash(t, m, "/yolo OFF")
	if len(saved) != 4 {
		t.Fatalf("saved = %+v; want four writes", saved)
	}
	for _, c := range saved[2:] {
		if c.Session.Mode() != config.PermissionModeAsk {
			t.Fatalf("`/yolo off` wrote %q, want ask", c.Session.Mode())
		}
	}
	if got := content(m); !strings.Contains(got, yoloOffNote) {
		t.Fatalf("expected the guardrails-on note after `/yolo OFF`, got:\n%s", got)
	}
}

// TestYoloRejectsAnUnknownArgument pins that a mistyped argument is REPORTED
// rather than silently treated as a toggle — on a guardrail switch, guessing is
// the wrong direction half the time.
func TestYoloRejectsAnUnknownArgument(t *testing.T) {
	var cfg config.Config
	var saved []config.Config
	m := newYoloApp(t, yoloEnv(&cfg, &saved))

	m = dispatchSlash(t, m, "/yolo maybe")
	if len(saved) != 0 {
		t.Fatalf("an unknown argument wrote config %+v; it must change nothing", saved)
	}
	got := content(m)
	if !strings.Contains(got, "/yolo takes on or off") || !strings.Contains(got, "maybe") {
		t.Fatalf("expected a complaint naming the argument, got:\n%s", got)
	}
}

// TestYoloKeyAndCommandProduceIdenticalState is the dual-binding contract: the
// key and the command are one action. Two apps, two routes, byte-identical
// config writes and status lines.
func TestYoloKeyAndCommandProduceIdenticalState(t *testing.T) {
	var keyCfg, cmdCfg config.Config
	var keySaved, cmdSaved []config.Config

	byKey := newYoloApp(t, yoloEnv(&keyCfg, &keySaved))
	byKey = press(t, byKey, tea.KeyPressMsg{Code: 'y', Mod: ctrl})

	byCommand := newYoloApp(t, yoloEnv(&cmdCfg, &cmdSaved))
	byCommand = dispatchSlash(t, byCommand, "/yolo")

	if len(keySaved) != 1 || len(cmdSaved) != 1 {
		t.Fatalf("writes: key=%d command=%d, want 1 each", len(keySaved), len(cmdSaved))
	}
	if !reflect.DeepEqual(keySaved[0], cmdSaved[0]) {
		t.Fatalf("ctrl+y wrote %+v but /yolo wrote %+v — the two bindings have drifted apart",
			keySaved[0], cmdSaved[0])
	}
	if key, cmd := content(byKey), content(byCommand); key != cmd {
		t.Fatalf("ctrl+y and /yolo left different frames:\n--- ctrl+y ---\n%s\n--- /yolo ---\n%s", key, cmd)
	}
	if !strings.Contains(content(byKey), yoloOnNote) {
		t.Fatalf("ctrl+y left no guardrails-off note; got:\n%s", content(byKey))
	}
}

// TestYoloKeyWorksOnEveryScreen pins "global": the binding is dispatched from
// the keymap table ahead of the per-screen handlers, so it reaches the toggle
// from the attach screen too, where a printable key would land in the input.
func TestYoloKeyWorksOnEveryScreen(t *testing.T) {
	var cfg config.Config
	var saved []config.Config
	m := newYoloApp(t, yoloEnv(&cfg, &saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session
	m = press(t, m, tea.KeyPressMsg{Code: 'y', Mod: ctrl})
	if got := content(m); !strings.Contains(got, yoloOnNote) {
		t.Fatalf("expected the guardrails-off note on the attach screen, got:\n%s", got)
	}

	if len(saved) != 1 || saved[0].Session.Mode() != config.PermissionModeYolo {
		t.Fatalf("ctrl+y on the attach screen wrote %+v; want one yolo write", saved)
	}
}

// TestYoloConfigReadFailureAbortsTheWrite mirrors applyModelSelection's own
// rule: a config that cannot be READ must not fall through to SaveConfig with a
// zero value, which would silently drop the user's permissions/telemetry
// blocks.
func TestYoloConfigReadFailureAbortsTheWrite(t *testing.T) {
	var saved []config.Config
	env := tui.GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return config.Config{}, errors.New("config.json: bad json") }
	env.SaveConfig = func(c config.Config) error {
		saved = append(saved, c)
		return nil
	}
	m := newYoloApp(t, env)

	m = dispatchSlash(t, m, "/yolo")
	if len(saved) != 0 {
		t.Fatalf("a failed config read still wrote %+v", saved)
	}
	if got := content(m); !strings.Contains(got, "couldn't load config") {
		t.Fatalf("expected the read failure on the status line, got:\n%s", got)
	}
}

// TestYoloNoteSeverities is the load-bearing color assertion, and the reason it
// renders through testkit.ColorTheme: theme.Test forces termenv.Ascii, which
// emits no escapes at all, so a plain golden is blind to severity by
// construction. Turning guardrails OFF must never be an unqualified green.
func TestYoloNoteSeverities(t *testing.T) {
	tests := []struct {
		name    string
		command string
		note    string
		tag     string
	}{
		{"turning guardrails off is a warning", "/yolo on", yoloOnNote, "yellow"},
		{"turning them back on is a success", "/yolo off", yoloOffNote, "green"},
		{"a bad argument is an error", "/yolo maybe", "/yolo takes on or off", "red"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg config.Config
			var saved []config.Config
			var m tea.Model = tui.NewApp(testkit.ColorTheme(), newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), yoloEnv(&cfg, &saved))
			m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m, _ = m.Update(m.Init()())
			m = dispatchSlash(t, m, tt.command)

			got := content(m)
			if !strings.Contains(got, tt.note) {
				t.Fatalf("test premise broken: %q is not on screen, so no severity is being asserted; got:\n%s", tt.note, got)
			}
			tagged := testkit.TagANSI(t, got)
			// Match the tag immediately followed by the note's head, so a
			// coincidental tag elsewhere in the frame can't satisfy this.
			head := tt.note
			if len([]rune(head)) > 30 {
				head = string([]rune(head)[:30])
			}
			if want := "<" + tt.tag + ">" + head; !strings.Contains(tagged, want) {
				t.Fatalf("expected %q in the %s style; tagged render:\n%s", tt.note, tt.tag, tagged)
			}
		})
	}
}

// TestYoloNotesFitTheWidthFloor measures both notes against the 80-column floor
// the golden tests pin. The status line is truncated to the terminal width
// (App.render), so a note that overruns silently reverts to whatever prefix
// survived — and for these two, the surviving prefix would be the claim without
// its "new sessions" qualification, which is exactly the overclaim the wording
// exists to avoid.
func TestYoloNotesFitTheWidthFloor(t *testing.T) {
	for _, tt := range []struct{ command, note string }{
		{"/yolo on", yoloOnNote},
		{"/yolo off", yoloOffNote},
	} {
		t.Run(tt.command, func(t *testing.T) {
			var cfg config.Config
			var saved []config.Config
			m := newYoloApp(t, yoloEnv(&cfg, &saved))
			m = dispatchSlash(t, m, tt.command)
			assertStatusFitsWidth(t, content(m), tt.note)
		})
	}
}

// TestYoloNotesClaimNothingAboutTheRunningSession is the honesty guard. gofer
// cannot change a live session's permission mode — the SDK fixes the guard at
// construction and carries no op to swap it (yolo.go) — so a note that reads as
// "guardrails are off now" would be a safety bug: the user would believe the
// session in front of them had been disarmed (or re-armed) when it had not.
func TestYoloNotesClaimNothingAboutTheRunningSession(t *testing.T) {
	for _, note := range []string{yoloOnNote, yoloOffNote} {
		if !strings.Contains(note, "new sessions") && !strings.Contains(note, "NEW sessions") {
			t.Errorf("note %q does not scope itself to new sessions", note)
		}
		if !strings.Contains(note, "running sessions keep theirs") {
			t.Errorf("note %q does not say the running session is unaffected", note)
		}
	}
}

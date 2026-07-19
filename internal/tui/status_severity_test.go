package tui_test

// status_severity_test.go covers issue #161: the transient status line used to
// render EVERY note in DangerStyle, so a successful /model change was painted
// the same red as an HTTP 400 and a user asked, of a change that had worked,
// "was that an error?".
//
// The oracle here is deliberately COLOR-AWARE. The plain testkit.AssertGolden
// harness renders through theme.Test()'s forced termenv.Ascii profile, which
// emits no escapes at all — a severity change is completely invisible to it, so
// an Ascii golden would be a vacuous assertion. These render through
// testkit.ColorTheme() and assert against *.styled.golden files, where the
// three severities appear as three distinct tags (<green>/<yellow>/<red>).
// Collapsing them back to one style fails all three.

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// statusSeverityCase is one reachable status note plus the style tag it must
// render in. Each drives the REAL App through real key presses to reach the
// note — no note is injected — so the mapping asserted is the one a user gets.
type statusSeverityCase struct {
	name string
	// golden is the *.styled.golden basename.
	golden string
	// tag is the testkit style tag the note must be wrapped in.
	tag string
	// note is the text expected on the status line, for a readable failure
	// before the golden diff.
	note string
	// drive reaches the note from a freshly built app.
	drive func(t *testing.T, sup *fakeSup, saved *[]config.Config) tea.Model
}

// statusSeverityCases is the ok/warn/danger set issue #161's acceptance
// criteria name one for one.
func statusSeverityCases() []statusSeverityCase {
	return []statusSeverityCase{
		{
			name:   "ok/a successful default-model change",
			golden: "app_status_severity_ok",
			tag:    "green",
			note:   "Default model set to Haiku 4.5.",
			drive: func(t *testing.T, sup *fakeSup, saved *[]config.Config) tea.Model {
				m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup, modelSelectEnv(saved))
				m = dispatchSlash(t, m, "/model")
				m = pressDown(t, m, pressesToHaiku)
				return press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			},
		},
		{
			name:   "warn/a caveat: the session kept its model",
			golden: "app_status_severity_warn",
			tag:    "yellow",
			note:   "Live model swap needs the same provider",
			drive: func(t *testing.T, sup *fakeSup, saved *[]config.Config) tea.Model {
				m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup, modelSelectEnv(saved))
				m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
				m = dispatchSlash(t, m, "/model")
				m = pressDown(t, m, pressesToGPT5)
				return press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			},
		},
		{
			name:   "danger/a genuinely failed operation",
			golden: "app_status_severity_danger",
			tag:    "red",
			note:   "gofer/set_model: session busy",
			drive: func(t *testing.T, sup *fakeSup, saved *[]config.Config) tea.Model {
				// The opDoneMsg error path — issue #161 asks for this to be the
				// ONLY route to danger for an op result.
				sup.setModelErr = errors.New("gofer/set_model: session busy")
				m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup, modelSelectEnv(saved))
				m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
				m = dispatchSlash(t, m, "/model")
				m = pressDown(t, m, pressesToHaiku)
				return press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			},
		},
	}
}

// TestGoldenStatusSeverityStyled is the styled-golden layer: a committed,
// reviewable capture of each severity's full rendered frame.
func TestGoldenStatusSeverityStyled(t *testing.T) {
	for _, tc := range statusSeverityCases() {
		t.Run(tc.name, func(t *testing.T) {
			var saved []config.Config
			sup := newFakeSup(modelSelectRoster())
			m := tc.drive(t, sup, &saved)
			testkit.AssertGoldenStyled(t, tc.golden, content(m))
		})
	}
}

// TestStatusSeverityStylesAreDistinct is the same three cases asserted
// directly, without a golden in the loop. It exists because a golden's failure
// mode is a wall of diff that says "something moved"; this says exactly which
// severity lost its color, and it cannot be quieted by a careless -update run.
//
// The final check is the load-bearing one: it proves the three notes render in
// three DIFFERENT styles. That is precisely the regression #161 describes —
// flattening them back to a single color — and no per-case assertion catches it
// on its own.
func TestStatusSeverityStylesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range statusSeverityCases() {
		t.Run(tc.name, func(t *testing.T) {
			var saved []config.Config
			sup := newFakeSup(modelSelectRoster())
			got := content(tc.drive(t, sup, &saved))

			if !strings.Contains(got, tc.note) {
				t.Fatalf("test premise broken: %q is not on screen, so no severity is being asserted; got:\n%s", tc.note, got)
			}
			want := "<" + tc.tag + ">"
			tagged := testkit.TagANSI(t, got)
			if !strings.Contains(tagged, want) {
				t.Fatalf("status note %q did not render in the %s style; tagged render:\n%s", tc.note, tc.tag, tagged)
			}
			seen[tc.tag] = tc.name
		})
	}
	if len(seen) != 3 {
		t.Fatalf("the three severities resolved to %d distinct styles (%v), want 3 — "+
			"collapsing ok/warn/danger to one color is exactly issue #161", len(seen), seen)
	}
}

// TestStatusSeverityResetsOnClear pins that clearing the status line resets its
// severity too. Without it a stale color outlives the note it described, and
// the next note written by a path that forgets a severity would inherit it —
// e.g. an error rendered green because the previous note happened to succeed.
func TestStatusSeverityResetsOnClear(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(content(m), "Default model set to") {
		t.Fatal("test premise broken: expected an ok note on screen first")
	}

	// Any key press clears the transient status line. ↓ rather than a
	// printable character on purpose: a character would land in the dispatch
	// bar's buffer and stop the "/nope" below from parsing as a slash command
	// at all.
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := content(m); strings.Contains(got, "Default model set to") {
		t.Fatalf("expected the status note to clear on the next key press, got:\n%s", got)
	}
	// An unknown command is a danger note; it must render red, not inherit the
	// green the cleared ok note left behind.
	m = dispatchSlash(t, m, "/nope")
	tagged := testkit.TagANSI(t, content(m))
	if !strings.Contains(tagged, "<red>unknown command: /nope") {
		t.Fatalf("expected the error note in the danger style after a cleared ok note; tagged render:\n%s", tagged)
	}
}

// TestDaemonProbeNotesFitTheWidthFloor measures every note issue #162's
// gofer/hello probe can produce against the 80-column floor the golden tests
// pin. The status line is truncated to the terminal width (App.render), so a
// note that overruns is not merely clipped — it silently reverts to whatever
// prefix survived, which for these notes is the unqualified half. A previous
// attempt at this feature shipped a suffix that busted 80 columns for exactly
// that reason.
//
// Measured, not eyeballed: each note is reached through the real select path
// and asserted to survive the real, already-truncated render intact.
func TestDaemonProbeNotesFitTheWidthFloor(t *testing.T) {
	tests := []struct {
		name string
		// daemonDefault is what the stub gofer/hello probe answers; equal to
		// the selected id means adopted, different means pinned.
		daemonDefault string
		attach        bool
		presses       int
		want          string
	}{
		{"overview/adopted", "claude-haiku-4-5", false, pressesToHaiku,
			"Default model saved; the attached daemon adopted it."},
		{"overview/pinned", "claude-opus-4-8", false, pressesToHaiku,
			"Default saved; the attached daemon is pinned to another model."},
		{"hot-swap/adopted", "claude-haiku-4-5", true, pressesToHaiku,
			"Model set for this session; the daemon took the new default."},
		{"hot-swap/pinned", "claude-opus-4-8", true, pressesToHaiku,
			"Model set for this session; the daemon is pinned to another default."},
		{"cross-provider/adopted", "gpt-5", true, pressesToGPT5,
			"Provider differs — session keeps its model; daemon took the default."},
		{"cross-provider/pinned", "claude-opus-4-8", true, pressesToGPT5,
			"Provider differs — session keeps its model; daemon is pinned."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var saved []config.Config
			var probes int
			sup := newFakeSup(modelSelectRoster())
			m := newModelSelectApp(t, sup, probingDaemonEnv(&saved, tt.daemonDefault, &probes))

			if tt.attach {
				m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
			}
			m = dispatchSlash(t, m, "/model")
			m = pressDown(t, m, tt.presses)
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

			if probes != 1 {
				t.Fatalf("gofer/hello probes = %d, want 1 — this case is not exercising the probe", probes)
			}
			got := content(m)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("expected note %q, got:\n%s", tt.want, got)
			}
			assertStatusFitsWidth(t, got, tt.want)
		})
	}
}

// TestGoldenCommandEnvHasNoDaemonProbe guards the fixture the whole golden
// suite renders against: GoldenCommandEnv must stay probe-free, so a golden can
// never depend on a network-shaped closure. cmd/gofer is the only place a real
// probe is wired.
func TestGoldenCommandEnvHasNoDaemonProbe(t *testing.T) {
	if env := tui.GoldenCommandEnv(); env.DaemonDefaultModel != nil || env.DaemonBacked {
		t.Fatalf("GoldenCommandEnv must stay local + probe-free; DaemonBacked=%v DaemonDefaultModel!=nil=%v",
			env.DaemonBacked, env.DaemonDefaultModel != nil)
	}
}

// TestDaemonProbeNoteSeverities is the color half of
// TestDaemonProbeNotesFitTheWidthFloor: that one pins WHAT each probe-derived
// note says, this one pins what each one MEANS.
//
// It exists because asserting only the text let a severity regression through
// undetected — downgrading the adopted note from ok to warn kept every other
// test in this package green. The rule is: the daemon ADOPTED the write and
// the session moved (or there was no session) → ok; anything the user might
// have expected to happen and did not — a pinned daemon, a session left on its
// old model — → warn. No probe outcome is danger; a genuine failure never gets
// this far (see TestModelSelectDaemonHotSwapFailureSkipsTheProbe).
func TestDaemonProbeNoteSeverities(t *testing.T) {
	tests := []struct {
		name          string
		daemonDefault string
		attach        bool
		presses       int
		want          string
		tag           string
	}{
		{"overview/adopted is a success", "claude-haiku-4-5", false, pressesToHaiku,
			"Default model saved; the attached daemon adopted it.", "green"},
		{"overview/pinned is a caveat", "claude-opus-4-8", false, pressesToHaiku,
			"Default saved; the attached daemon is pinned to another model.", "yellow"},
		{"hot-swap/adopted is a success", "claude-haiku-4-5", true, pressesToHaiku,
			"Model set for this session; the daemon took the new default.", "green"},
		{"hot-swap/pinned is a caveat", "claude-opus-4-8", true, pressesToHaiku,
			"Model set for this session; the daemon is pinned to another default.", "yellow"},
		{"cross-provider/adopted is a caveat", "gpt-5", true, pressesToGPT5,
			"Provider differs — session keeps its model; daemon took the default.", "yellow"},
		{"cross-provider/pinned is a caveat", "claude-opus-4-8", true, pressesToGPT5,
			"Provider differs — session keeps its model; daemon is pinned.", "yellow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var saved []config.Config
			var probes int
			sup := newFakeSup(modelSelectRoster())
			m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup,
				probingDaemonEnv(&saved, tt.daemonDefault, &probes))

			if tt.attach {
				m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
			}
			m = dispatchSlash(t, m, "/model")
			m = pressDown(t, m, tt.presses)
			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

			if probes != 1 {
				t.Fatalf("gofer/hello probes = %d, want 1 — this case is not exercising the probe", probes)
			}
			tagged := testkit.TagANSI(t, content(m))
			// The note is width-truncated in the render, so match on a prefix
			// that survives at 80 columns rather than the whole string.
			head := string([]rune(tt.want)[:40])
			if want := "<" + tt.tag + ">" + head; !strings.Contains(tagged, want) {
				t.Fatalf("expected %q to render in the %s style; tagged render:\n%s", tt.want, tt.tag, tagged)
			}
		})
	}
}

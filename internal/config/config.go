// Package config is gofer's native on-disk configuration. M3 defined the
// permissions block — the ruleset gofer's guard consults before it runs a tool
// call. M4 step 3 adds Session/TUI (new-session and UI defaults) plus [Save],
// and the parallel settings registry in internal/tui that the /config command
// panel view reads and writes through. A vendor-format import (Claude Code
// settings.json) is deliberately NOT here: that lands in M4/M5 (see the SDK's
// permission package doc). More config sections (plugins, …) join this type in
// later milestones.
//
// The file format is JSON, read from <root>/config.json (see [DefaultPath]).
// A missing file is not an error — an unconfigured gofer runs the default
// contain-or-ask policy (see [Config.Engine]).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/permission"

	"github.com/jedwards1230/gofer/internal/telemetry"
)

// ConfigFileName is the base name of gofer's config file under the store root.
const ConfigFileName = "config.json"

// Config is gofer's parsed configuration file.
type Config struct {
	// Permissions is the native permission ruleset, evaluated with the SDK's
	// deny>ask>allow precedence on top of gofer's default contain-or-ask
	// catch-all (see [Config.Engine]).
	Permissions []Rule `json:"permissions,omitempty"`
	// Telemetry is gofer's OpenTelemetry configuration block. The zero value
	// is disabled — see [Telemetry.ToTelemetry].
	Telemetry Telemetry `json:"telemetry,omitempty"`
	// Session holds defaults for new sessions (model, permission mode). The
	// zero value means "unset" — the TUI's settings registry resolves each
	// field's own default (see internal/tui's settings registry).
	Session Session `json:"session,omitempty"`
	// TUI holds gofer's own UI preferences, as opposed to Session's
	// new-session defaults.
	TUI TUI `json:"tui,omitempty"`
	// Daemon holds daemon-process (as opposed to per-session) lifecycle
	// preferences. The zero value means "unset" — each field resolves to its own
	// default.
	Daemon Daemon `json:"daemon,omitempty"`
}

// Daemon holds daemon-process lifecycle preferences, distinct from Session's
// new-session defaults: they tune the daemon itself, not the sessions it hosts.
// The zero value is fully valid — every field resolves to a built-in default.
type Daemon struct {
	// DrainTimeoutMS bounds, in milliseconds, how long a `--workers` daemon's
	// graceful shutdown waits for in-flight turns to finish settling on their
	// workers before it detaches (see the router's Drain and cmd/gofer's
	// serveDaemonForeground). This is the M6 hot-upgrade drain window: the daemon
	// stops admitting new sessions and lets running turns reach idle so it stays
	// attached — relaying their events — until they finish, rather than detaching
	// mid-turn. On timeout the daemon detaches anyway; the detached workers keep
	// running and are re-adopted on the next start (design §3), so the bound
	// trades a longer clean-shutdown wait against a snappier exit. nil (unset)
	// resolves to [DefaultDrainTimeout]; a value <= 0 also resolves to the default
	// (a zero-length drain would defeat the purpose). See [Daemon.DrainTimeout].
	DrainTimeoutMS *int `json:"drain_timeout_ms,omitempty"`
}

// DefaultDrainTimeout is [Daemon.DrainTimeoutMS]'s default: 30s. Long enough for
// a typical in-flight turn to reach idle so a graceful shutdown drains cleanly,
// short enough that a session genuinely wedged mid-turn (e.g. one blocked on a
// permission that will never be answered during shutdown) does not hold the exit
// open indefinitely — the daemon detaches and the worker is re-adopted next
// start regardless.
const DefaultDrainTimeout = 30 * time.Second

// DrainTimeout resolves [Daemon.DrainTimeoutMS]'s effective value:
// [DefaultDrainTimeout] when unset or non-positive, else the explicit
// millisecond bound.
func (d Daemon) DrainTimeout() time.Duration {
	if d.DrainTimeoutMS == nil || *d.DrainTimeoutMS <= 0 {
		return DefaultDrainTimeout
	}
	return time.Duration(*d.DrainTimeoutMS) * time.Millisecond
}

// Session holds the defaults a new session is created with. The zero value
// means "unset" — Model resolves to the credential-driven default
// ([runner.DefaultModel]) and PermissionMode to "ask", the same contain-or-ask
// posture [Config.Engine] already defaults to.
type Session struct {
	// Model is the default model id for new sessions. Empty means
	// credential-driven (see runner.DefaultModel) rather than a fixed model.
	Model string `json:"model,omitempty"`

	// Effort is the default reasoning effort for new sessions: "low",
	// "medium", "high", or empty for the provider's own default (the SDK's
	// unified vocabulary — see provider.ValidEffort). Empty is a real,
	// supported value here rather than merely "unset": there is no separate
	// "no opinion" state to distinguish, since clearing the level IS asking
	// for the provider default. Written by the TUI's `/thinking` command,
	// which spells the empty value "off".
	//
	// NOT YET CONSUMED at session creation — the same status
	// [Session.PermissionMode] carries, and the reason the /thinking picker's
	// ✓ deliberately does NOT read it (internal/tui's effortPickerView.
	// activeEffort): nothing populates a session's Params.Thinking, so a level
	// stored here reaches no runner and claiming it as active would be a
	// fiction. Wiring it in is blocked on more than plumbing — see docs/TUI.md's
	// "reasoning effort" note: the SDK's per-turn overlay sets
	// Params.Thinking.Effort but never Params.Thinking.Enabled, and both
	// provider adapters emit reasoning config only when Enabled is true.
	Effort string `json:"effort,omitempty"`
	// PermissionMode is the default guardrail mode for new sessions: "ask"
	// (contain-or-ask, the default) or "yolo". Not yet consumed by
	// [Config.Engine] — it is a settings-registry knob today; wiring it into
	// session creation lands with /yolo (see docs/TUI.md).
	PermissionMode string `json:"permission_mode,omitempty"`

	// MaxSubagentDepth caps how deep a subagent session tree may nest: a root
	// session is depth 0, its child 1, and a Create naming a parent already at
	// this depth is refused with [supervisor.ErrDepthExceeded]. It is the one
	// guard against a runaway spawn chain, and it is config rather than a
	// literal because the useful depth is a workflow opinion, not a property of
	// gofer. Unset (0) — and any negative value, which is meaningless as a cap —
	// resolves to [DefaultMaxSubagentDepth]; zero deliberately does NOT mean "no
	// children allowed", so an existing config file keeps working unchanged. See
	// [Session.SubagentDepthLimit].
	MaxSubagentDepth int `json:"max_subagent_depth,omitempty"`

	// LoadSettleTimeoutMS bounds, in milliseconds, how long session/load waits
	// for a live session's in-flight turn to finish journaling before it folds
	// and replays history (see the daemon's handleSessionLoad and issue #137). A
	// turn's assistant/tool entries are journaled ASYNCHRONOUSLY after the
	// turn.finished event a client observes, so a load landing in that window
	// would otherwise read — and silently replay — a SHORT history. The load
	// waits (best-effort) for the session to report needs-input, the observable
	// signal that the journal barrier has passed. nil (unset) resolves to
	// [DefaultLoadSettleTimeout]; a value <= 0 also resolves to the default (the
	// wait is always on — the timeout only bounds a session genuinely mid-turn,
	// e.g. one blocked on a permission, which never settles). See
	// [Session.LoadSettleTimeout].
	LoadSettleTimeoutMS *int `json:"load_settle_timeout_ms,omitempty"`
}

// DefaultLoadSettleTimeout is [Session.LoadSettleTimeoutMS]'s default: 2s. The
// journaling-flush window session/load waits out closes in milliseconds, so a
// short bound closes the incomplete-history race (issue #137) while still
// letting a load of a session genuinely mid-turn — one that will never reach
// needs-input, e.g. an adopted worker blocked on a permission (design §7) —
// fall through to fold whatever is durable on disk rather than deadlocking.
const DefaultLoadSettleTimeout = 2 * time.Second

// LoadSettleTimeout resolves [Session.LoadSettleTimeoutMS]'s effective value:
// [DefaultLoadSettleTimeout] when unset or non-positive, else the explicit
// millisecond bound.
func (s Session) LoadSettleTimeout() time.Duration {
	if s.LoadSettleTimeoutMS == nil || *s.LoadSettleTimeoutMS <= 0 {
		return DefaultLoadSettleTimeout
	}
	return time.Duration(*s.LoadSettleTimeoutMS) * time.Millisecond
}

// DefaultMaxSubagentDepth is [Session.MaxSubagentDepth]'s default: 5. Deep
// enough for the delegation chains a supervising agent actually builds
// (owner → worker → helper), shallow enough that a spawn loop is caught within
// a handful of sessions rather than after it has filled the store.
const DefaultMaxSubagentDepth = 5

// SubagentDepthLimit resolves [Session.MaxSubagentDepth]'s effective value:
// [DefaultMaxSubagentDepth] when unset or non-positive, else the explicit cap.
func (s Session) SubagentDepthLimit() int {
	if s.MaxSubagentDepth <= 0 {
		return DefaultMaxSubagentDepth
	}
	return s.MaxSubagentDepth
}

// TUI holds gofer's own interface preferences, distinct from Session's
// new-session defaults.
type TUI struct {
	// RosterView selects the overview's default row ordering: "flat"
	// (recency across the whole roster, the default) or "grouped" (by
	// status). Mirrors the `tab` key's [Overview.ToggleView] toggle.
	RosterView string `json:"roster_view,omitempty"`

	// Autoscroll controls whether the attach screen's transcript auto-tails
	// new streaming content: nil (unset) or true is the default — the
	// transcript stays pinned to the latest message as it streams in,
	// scrolling the header/oldest messages away exactly like before this
	// setting existed. An explicit false is "manual": new events never move
	// the view — it stays wherever the operator last left it (wheel/PgUp/
	// PgDn) — until they scroll back down themselves. A *bool, not a plain
	// bool, is required: JSON can't distinguish "field absent" from "field
	// explicitly false" on a plain bool (both marshal from, and unmarshal
	// to, the zero value), so an explicit false would silently come back as
	// the default true on the next Load. See [TUI.AutoscrollEnabled] for the
	// resolved value every caller should read instead of this field
	// directly.
	Autoscroll *bool `json:"autoscroll,omitempty"`

	// Mouse controls whether the TUI enables mouse capture (cell-motion
	// reporting) at all: nil (unset) or true is the default — mouse-wheel
	// scroll and app-owned click-drag text selection with OSC 52 copy are
	// both live. An explicit false disables mouse capture altogether
	// (App.View sets tea.MouseModeNone instead of tea.MouseModeCellMotion),
	// handing control back to the terminal's own native click-to-select and
	// native scroll — the escape hatch for a terminal where OSC 52 or SGR
	// mouse reporting misbehaves. Same *bool rationale as [TUI.Autoscroll]:
	// a plain bool can't distinguish "unset" from "explicitly false". See
	// [TUI.MouseEnabled] for the resolved value every caller should read.
	Mouse *bool `json:"mouse,omitempty"`

	// MaxPasteBytes caps how much bracketed-paste content one paste may
	// insert into a text-entry surface: nil (unset) is the default
	// [DefaultMaxPasteBytes], an explicit 0 is "no limit", and any other
	// value is a byte cap. The cap exists because a pasted buffer is a plain
	// Go string that the whole input line is re-derived from on EVERY frame
	// (rune-slicing plus a display-width measure — see internal/tui's
	// inputBuffer.Render), so an accidental multi-megabyte paste turns every
	// redraw into megabytes of allocation and wedges the UI, with the
	// content itself unreadable in a one-line input anyway. A paste over the
	// cap is clipped on a rune boundary and reported on the status line —
	// never dropped silently. A *int, not a plain int, for the same reason
	// [TUI.Autoscroll] is a *bool: a plain int can't distinguish "field
	// absent" (use the default) from an explicit 0 ("no limit"). See
	// [TUI.PasteLimitBytes] for the resolved value every caller should read.
	MaxPasteBytes *int `json:"max_paste_bytes,omitempty"`

	// ShellTimeoutMS bounds, in milliseconds, how long a child process the
	// TUI spawns for an INPUT PREFIX may run before it is killed: the `!` /
	// `!!` shell escape's command, and the `git ls-files` enumeration `@`
	// file-mention completion uses (see internal/tui's shell.go and
	// filemention.go). A shell escape is a foreground affordance — the user
	// is staring at a TUI that cannot repaint the result until the command
	// exits — so an unbounded `!` is a wedged UI, not a long job. nil (unset)
	// or a non-positive value resolves to [DefaultShellTimeout]. See
	// [TUI.ShellTimeout].
	ShellTimeoutMS *int `json:"shell_timeout_ms,omitempty"`

	// ShellMaxOutputBytes caps how much combined stdout+stderr one `!` / `!!`
	// shell escape may retain: nil (unset) is [DefaultShellMaxOutputBytes],
	// an explicit 0 is "no limit", and any other value is a byte cap. Output
	// past the cap is dropped with a visible truncation marker rather than
	// silently — and the child keeps running (its writes are accepted and
	// discarded) rather than dying on a broken pipe. The cap exists for two
	// reasons at once: the pane re-renders the retained output on every
	// frame, and a `!` run's output is folded into the NEXT prompt's context
	// (context-cost discipline — a stray `!cat huge.log` must not silently
	// buy the user a megabyte of tokens). Same *int rationale as
	// [TUI.MaxPasteBytes]. See [TUI.ShellOutputLimitBytes].
	ShellMaxOutputBytes *int `json:"shell_max_output_bytes,omitempty"`

	// FileMentionMaxEntries bounds how many paths the `@` file-mention
	// completion's directory walk collects before it stops: nil (unset) or a
	// non-positive value resolves to [DefaultFileMentionMaxEntries]. The
	// bound is what keeps `@` usable in a tree the walk has no business
	// enumerating in full (a vendored monorepo, a home directory); the
	// enumeration runs off the Update loop either way, so this bounds memory
	// and match time, not responsiveness. See [TUI.FileMentionEntryLimit].
	FileMentionMaxEntries *int `json:"file_mention_max_entries,omitempty"`

	// FileMentionMaxDepth bounds how deep below the session's cwd the `@`
	// file-mention completion's fallback directory walk descends (1 = the cwd's
	// own entries only): nil (unset) or a non-positive value resolves to
	// [DefaultFileMentionMaxDepth]. It applies to the WalkDir fallback only —
	// inside a git repository the candidates come from `git ls-files`, which
	// has no depth notion (and honors .gitignore, which the walk cannot). See
	// [TUI.FileMentionDepthLimit].
	FileMentionMaxDepth *int `json:"file_mention_max_depth,omitempty"`

	// ApprovalBodyLines caps how many rows the inline approval prompt spends
	// on the gated call's own body — the command text plus the residual
	// spec `k=v` lines (see internal/tui's renderApprovalPrompt): nil (unset)
	// is the default [DefaultApprovalBodyLines], and any positive value is a
	// row cap, with the overflow collapsed into a single "… +N more lines"
	// row. The cap exists because the prompt commandeers the whole footer:
	// a pasted 200-line heredoc would otherwise push the question and the
	// action row off the top of the frame, leaving an operator staring at a
	// wall of script with no visible way to answer it. A *int, not a plain
	// int, for the same reason [TUI.Autoscroll] is a *bool — a plain int
	// can't distinguish "field absent" from an explicit 0. See
	// [TUI.ApprovalBodyLineLimit] for the resolved value every caller should
	// read.
	ApprovalBodyLines *int `json:"approval_body_lines,omitempty"`

	// ApprovalMinTranscriptRows is how many transcript rows the inline
	// approval prompt must leave visible above itself: nil (unset) is the
	// default [DefaultApprovalMinTranscriptRows], and any positive value is a
	// floor. When the full prompt would leave fewer rows than this, its
	// rationale collapses to the opening paragraph plus a "… ctrl+e to
	// explain" pointer (see internal/tui's renderApprovalPrompt); the header,
	// the gated call's body, the question, and the action row never collapse.
	// The floor exists because the prompt commandeers the whole footer: at a
	// 24-row terminal the full block leaves a two-line transcript, so the
	// conversation that led to the gated call — the context a decision is
	// actually made on — scrolls out of view exactly when it is needed. A
	// *int, not a plain int, for the same reason [TUI.Autoscroll] is a *bool.
	// See [TUI.ApprovalMinTranscriptRowFloor] for the resolved value every
	// caller should read.
	ApprovalMinTranscriptRows *int `json:"approval_min_transcript_rows,omitempty"`

	// MaxCommandFileBytes caps how large one user markdown command file
	// (`<root>/commands/*.md`, `<cwd>/.gofer/commands/*.md` — see
	// internal/usercmd) may be: nil (unset) is the default
	// [DefaultMaxCommandFileBytes], an explicit 0 is "no limit", and any other
	// value is a byte cap. A file over the cap is skipped with a status note,
	// never truncated — half a prompt is not a prompt, and a command's body is
	// submitted to the model verbatim. The cap exists because the commands
	// directories are walked and read whole on the `/` keypress, so one
	// stray multi-megabyte file (a log accidentally saved as `.md`) would
	// otherwise be read into memory on every popup open and, if run, sent to a
	// provider. A *int, not a plain int, for the same reason
	// [TUI.Autoscroll] is a *bool: a plain int can't distinguish "field
	// absent" from an explicit 0. See [TUI.CommandFileLimitBytes] for the
	// resolved value every caller should read.
	MaxCommandFileBytes *int `json:"max_command_file_bytes,omitempty"`
}

// DefaultMaxCommandFileBytes is [TUI.MaxCommandFileBytes]'s default: 256 KiB.
// Far above any hand-written prompt file (a 256 KiB prompt is already past
// most models' context windows) and far below the size at which reading one
// on a keypress is noticeable.
const DefaultMaxCommandFileBytes = 256 << 10

// CommandFileLimitBytes resolves [TUI.MaxCommandFileBytes]'s effective value:
// [DefaultMaxCommandFileBytes] when unset, else the explicit stored value
// (0 = no limit). A negative stored value is meaningless as a cap and
// resolves to the default rather than skipping every command file.
func (t TUI) CommandFileLimitBytes() int {
	if t.MaxCommandFileBytes == nil || *t.MaxCommandFileBytes < 0 {
		return DefaultMaxCommandFileBytes
	}
	return *t.MaxCommandFileBytes
}

// DefaultShellTimeout is [TUI.ShellTimeoutMS]'s default: 30s. Long enough for
// the builds, greps, and git commands a `!` escape is actually for, short
// enough that a command that will never exit (an accidental `!tail -f`, a
// process waiting on stdin the TUI does not give it one of) frees the pane
// on its own instead of stranding it "running…" forever.
const DefaultShellTimeout = 30 * time.Second

// ShellTimeout resolves [TUI.ShellTimeoutMS]'s effective value:
// [DefaultShellTimeout] when unset or non-positive, else the explicit
// millisecond bound.
func (t TUI) ShellTimeout() time.Duration {
	if t.ShellTimeoutMS == nil || *t.ShellTimeoutMS <= 0 {
		return DefaultShellTimeout
	}
	return time.Duration(*t.ShellTimeoutMS) * time.Millisecond
}

// DefaultShellMaxOutputBytes is [TUI.ShellMaxOutputBytes]'s default: 64 KiB.
// Comfortably more than the screenful a human reads back off a `!` escape,
// and already a four-figure token count if the run is folded into the next
// prompt — which is the side that sets the ceiling, not the rendering.
const DefaultShellMaxOutputBytes = 64 << 10

// ShellOutputLimitBytes resolves [TUI.ShellMaxOutputBytes]'s effective value:
// [DefaultShellMaxOutputBytes] when unset, else the explicit stored value
// (0 = no limit). A negative stored value is meaningless as a cap and
// resolves to the default rather than discarding all output.
func (t TUI) ShellOutputLimitBytes() int {
	if t.ShellMaxOutputBytes == nil || *t.ShellMaxOutputBytes < 0 {
		return DefaultShellMaxOutputBytes
	}
	return *t.ShellMaxOutputBytes
}

// DefaultFileMentionMaxEntries is [TUI.FileMentionMaxEntries]'s default:
// 5000 paths — more than the file count of most single projects, and small
// enough that filtering it per keystroke stays imperceptible.
const DefaultFileMentionMaxEntries = 5000

// FileMentionEntryLimit resolves [TUI.FileMentionMaxEntries]'s effective
// value: [DefaultFileMentionMaxEntries] when unset or non-positive.
func (t TUI) FileMentionEntryLimit() int {
	if t.FileMentionMaxEntries == nil || *t.FileMentionMaxEntries <= 0 {
		return DefaultFileMentionMaxEntries
	}
	return *t.FileMentionMaxEntries
}

// DefaultFileMentionMaxDepth is [TUI.FileMentionMaxDepth]'s default: 8
// directory levels below the cwd, which reaches the leaves of an ordinary
// source tree without descending into the arbitrarily deep nesting a
// dependency directory can carry.
const DefaultFileMentionMaxDepth = 8

// FileMentionDepthLimit resolves [TUI.FileMentionMaxDepth]'s effective value:
// [DefaultFileMentionMaxDepth] when unset or non-positive.
func (t TUI) FileMentionDepthLimit() int {
	if t.FileMentionMaxDepth == nil || *t.FileMentionMaxDepth <= 0 {
		return DefaultFileMentionMaxDepth
	}
	return *t.FileMentionMaxDepth
}

// DefaultMaxPasteBytes is [TUI.MaxPasteBytes]'s default: 128 KiB, comfortably
// above any prompt a human pastes into a one-line input (a 128 KiB prompt is
// already tens of thousands of tokens) while still well below the size at
// which per-frame re-rendering of the buffer becomes visible latency.
const DefaultMaxPasteBytes = 128 << 10

// PasteLimitBytes resolves [TUI.MaxPasteBytes]'s effective value:
// [DefaultMaxPasteBytes] when unset, else the explicit stored value (0 = no
// limit). A negative stored value is meaningless as a cap and resolves to the
// default rather than clipping every paste to nothing.
func (t TUI) PasteLimitBytes() int {
	if t.MaxPasteBytes == nil || *t.MaxPasteBytes < 0 {
		return DefaultMaxPasteBytes
	}
	return *t.MaxPasteBytes
}

// DefaultApprovalBodyLines is [TUI.ApprovalBodyLines]'s default: 12 rows.
// Enough to read a realistic multi-line shell command or a short heredoc
// whole, while leaving the rationale, the question, and the action row on
// screen at the 24-row terminal height gofer's own golden renders assume.
const DefaultApprovalBodyLines = 12

// ApprovalBodyLineLimit resolves [TUI.ApprovalBodyLines]'s effective value:
// [DefaultApprovalBodyLines] when unset, and also for a stored value <= 0 —
// unlike [TUI.PasteLimitBytes]'s 0-means-unlimited, a zero-row body would
// render the prompt with the gated call itself invisible, which is never what
// an operator means.
func (t TUI) ApprovalBodyLineLimit() int {
	if t.ApprovalBodyLines == nil || *t.ApprovalBodyLines <= 0 {
		return DefaultApprovalBodyLines
	}
	return *t.ApprovalBodyLines
}

// DefaultApprovalMinTranscriptRows is [TUI.ApprovalMinTranscriptRows]'s
// default: 8 rows. Enough for the last exchange that led to the gated call to
// stay readable beside the prompt on the 24-row terminal gofer's own golden
// renders assume, and small enough that a comfortable terminal never collapses
// the rationale at all.
const DefaultApprovalMinTranscriptRows = 8

// ApprovalMinTranscriptRowFloor resolves [TUI.ApprovalMinTranscriptRows]'s
// effective value: [DefaultApprovalMinTranscriptRows] when unset, and also for
// a negative stored value. An explicit 0 IS honored — unlike
// [TUI.ApprovalBodyLineLimit]'s zero-means-default, "reserve no transcript" is
// a coherent preference (always show the whole prompt, whatever the frame
// height), so a user who asks for it gets it.
func (t TUI) ApprovalMinTranscriptRowFloor() int {
	if t.ApprovalMinTranscriptRows == nil || *t.ApprovalMinTranscriptRows < 0 {
		return DefaultApprovalMinTranscriptRows
	}
	return *t.ApprovalMinTranscriptRows
}

// AutoscrollEnabled resolves [TUI.Autoscroll]'s effective value: true (the
// default) when unset, else the explicit stored value.
func (t TUI) AutoscrollEnabled() bool {
	return t.Autoscroll == nil || *t.Autoscroll
}

// MouseEnabled resolves [TUI.Mouse]'s effective value: true (the default)
// when unset, else the explicit stored value.
func (t TUI) MouseEnabled() bool {
	return t.Mouse == nil || *t.Mouse
}

// Telemetry is gofer's native OpenTelemetry configuration block, mirroring
// [telemetry.Config]'s fields for JSON persistence. The zero value is fully
// valid and disabled (see [Telemetry.ToTelemetry]) — an unconfigured gofer
// exports no traces or metrics.
type Telemetry struct {
	Enabled     bool              `json:"enabled,omitempty"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Protocol    string            `json:"protocol,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
	Insecure    bool              `json:"insecure,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// ToTelemetry converts the config block into a [telemetry.Config]. Follows
// the same "zero value yields a sane (here: disabled) default" pattern as
// [Config.Engine] — a config file with no telemetry block, or no config file
// at all, compiles to telemetry.Config{}, which [telemetry.Setup] treats as
// off.
func (t Telemetry) ToTelemetry() telemetry.Config {
	return telemetry.Config{
		Enabled:     t.Enabled,
		Endpoint:    t.Endpoint,
		Protocol:    t.Protocol,
		ServiceName: t.ServiceName,
		Insecure:    t.Insecure,
		Headers:     t.Headers,
	}
}

// Rule is one native permission rule: a Verdict (allow|ask|deny) applied to a
// Tool + Specifier. Tool ""/"*" matches any tool; Specifier ""/"*" matches any
// target, a "prefix:*" specifier matches by command/target prefix, otherwise it
// is a path.Match glob — the SDK's [permission.Rule] grammar this compiles to.
type Rule struct {
	Verdict   string `json:"verdict"`
	Tool      string `json:"tool,omitempty"`
	Specifier string `json:"specifier,omitempty"`
}

// DefaultPath returns the config file path for a store root: <root>/config.json.
func DefaultPath(root string) string { return filepath.Join(root, ConfigFileName) }

// Load reads and parses the gofer config file at path. A missing file is NOT an
// error: it returns the zero Config, whose [Config.Engine] is the default
// contain-or-ask policy, so an unconfigured gofer still runs. A present but
// malformed or invalid file IS an error — a typo in a permission rule must fail
// loudly rather than silently widening or narrowing the policy.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path as indented JSON, atomically: it marshals to a temp
// file in the same directory (so the rename below is same-filesystem, hence
// atomic) with mode 0600 — gofer's config can carry a session.model default
// and other operator preferences, not a secret, but 0600 keeps it consistent
// with the rest of gofer's on-disk store — then renames it over path. A
// reader (Load) never observes a partially written file: either the old
// contents or the new ones, never a half-write. The parent directory is
// created if missing, matching the store root gofer already creates on first
// use.
func Save(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Clean up the temp file on any early return; a successful Rename below
	// moves it into place first, so this Remove after a clean run is a no-op
	// (the path no longer exists under tmpPath).
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: rename %s to %s: %w", tmpPath, path, err)
	}
	return nil
}

// validate rejects a rule with an unrecognized verdict, so a typo ("den")
// surfaces at load rather than silently never matching.
func (c Config) validate() error {
	for i, r := range c.Permissions {
		switch event.Verdict(r.Verdict) {
		case event.VerdictAllow, event.VerdictAsk, event.VerdictDeny:
		default:
			return fmt.Errorf("permissions[%d]: unknown verdict %q (want allow, ask, or deny)", i, r.Verdict)
		}
	}
	return nil
}

// Engine compiles the config into an SDK [permission.Engine] carrying gofer's
// default policy: contain-or-ask.
//
// A catch-all allow rule is seeded FIRST, so a call no config rule matches
// resolves to allow — which the guard's [loop.RuleGuard] then routes through the
// sandbox Container (run-contained when containable, else ask a human). The
// config's own rules are appended after; because the engine evaluates deny
// before ask before allow, a config deny or ask rule for a given tool+specifier
// wins over the default catch-all allow, while unmatched calls keep the
// contain-or-ask default. An empty config therefore yields "allow everything →
// contain-or-ask", never "run everything uncontained".
func (c Config) Engine() *permission.Engine {
	rules := make([]permission.Rule, 0, len(c.Permissions)+1)
	rules = append(rules, permission.Rule{
		Verdict:   event.VerdictAllow,
		Tool:      "*",
		Specifier: "*",
		Source:    "default",
	})
	for _, r := range c.Permissions {
		rules = append(rules, permission.Rule{
			Verdict:   event.Verdict(r.Verdict),
			Tool:      r.Tool,
			Specifier: r.Specifier,
			Source:    "config",
		})
	}
	return permission.New(rules...)
}

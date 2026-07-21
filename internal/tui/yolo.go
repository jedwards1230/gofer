package tui

// yolo.go is the /yolo guardrail toggle: ONE commit path
// ([App.applyPermissionMode]) that both the slash command (command.go's
// runYolo) and the ctrl+y key (keymap.go's global row) route through, so the
// two can never drift into different persistence, different wording, or
// different severities.
//
// WHAT IT CHANGES, precisely. It writes `session.permission_mode` to
// config.json through the same [CommandEnv.Config]/[CommandEnv.SaveConfig] pair
// /model's applyModelSelection uses. That value is resolved PER SESSION
// CREATION by whichever supervisor is running (cmd/gofer's
// permissionModeResolver → supervisor.Config.PermissionMode), so the next
// session started — local backend or attached daemon — gets the new posture
// with no restart.
//
// WHAT IT DOES NOT CHANGE: a session that is already running. The SDK fixes a
// session's guard at construction (runner.Options.Guard) and carries no
// contract op for changing it — there is no session.set_permission_mode
// alongside session.set_model, and Runner exposes no SetGuard. Reaching past
// the contract to swap it anyway would violate gofer's first architecture
// invariant (CLAUDE.md), so the toggle is honest about its reach instead: every
// status note below says "new sessions", and neither says "this session".

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
)

// permissionModeTarget is what a /yolo invocation resolves to: the bare form
// flips whatever is persisted, `on`/`off` state it outright so a user who is
// unsure of the current posture can be unambiguous.
type permissionModeTarget int

const (
	yoloToggle permissionModeTarget = iota
	yoloOn
	yoloOff
)

// resolve maps the target plus the currently persisted mode to the mode to
// write.
func (t permissionModeTarget) resolve(current config.PermissionMode) config.PermissionMode {
	switch t {
	case yoloOn:
		return config.PermissionModeYolo
	case yoloOff:
		return config.PermissionModeAsk
	default:
		if current == config.PermissionModeYolo {
			return config.PermissionModeAsk
		}
		return config.PermissionModeYolo
	}
}

// parseYoloArgs maps /yolo's arguments to a target. Bare is a toggle; a single
// on/off (case-insensitive) states the posture; anything else is rejected by
// name — silently toggling on an argument the user clearly meant as an
// instruction is exactly the args-discarding defect issue #165 describes, and
// on a guardrail switch it would also be the wrong direction half the time.
func parseYoloArgs(args []string) (permissionModeTarget, string) {
	switch len(args) {
	case 0:
		return yoloToggle, ""
	case 1:
		switch strings.ToLower(args[0]) {
		case "on", "yolo":
			return yoloOn, ""
		case "off", "ask":
			return yoloOff, ""
		}
		return yoloToggle, "/yolo takes on or off — got " + strconv.Quote(args[0])
	default:
		return yoloToggle, "/yolo takes at most one argument — got " + strconv.Itoa(len(args))
	}
}

// runYolo is /yolo's [Command.Run].
func runYolo(a App, args []string) (App, tea.Cmd) {
	target, argErr := parseYoloArgs(args)
	if argErr != "" {
		a.setStatus(sevDanger, argErr)
		return a, nil
	}
	return a.applyPermissionMode(target)
}

// applyPermissionMode is the single commit path: read the persisted config,
// resolve the new mode, write it back, and report what actually changed.
//
// The severities are the point of the wording. Turning guardrails OFF is never
// a plain success — the action worked, but the user is now in a posture where
// tool calls run unattended and uncontained, which is precisely [sevWarn]'s
// "the action did something, but not everything the user might expect"
// register; painting it the same green as "model set" would make the most
// consequential toggle in the TUI the least visible. Turning them back ON is an
// unqualified [sevOK]. A failed read or write is [sevDanger], and — mirroring
// applyModelSelection — a read failure ABORTS rather than falling through to
// SaveConfig with a zero-value config, which would silently drop the user's
// permissions/telemetry blocks.
func (a App) applyPermissionMode(target permissionModeTarget) (App, tea.Cmd) {
	var cfg config.Config
	if a.commandEnv.Config != nil {
		c, err := a.commandEnv.Config()
		if err != nil {
			a.setStatus(sevDanger, "couldn't load config: "+err.Error())
			return a, nil
		}
		cfg = c
	}

	next := target.resolve(cfg.Session.Mode())
	cfg.Session.PermissionMode = string(next)
	if a.commandEnv.SaveConfig != nil {
		if err := a.commandEnv.SaveConfig(cfg); err != nil {
			a.setStatus(sevDanger, "couldn't save permission mode: "+err.Error())
			return a, nil
		}
	}

	// The panel (if one is open) captured its config working copy at open time,
	// so a toggle underneath it would leave /config showing the stale value.
	// Close it — the toggle is a committing action, same as a /model select.
	a.panel = nil

	if next == config.PermissionModeYolo {
		a.setStatus(sevWarn, yoloOnNote)
	} else {
		a.setStatus(sevOK, yoloOffNote)
	}
	return a, nil
}

// The two notes, as constants so the width test can measure them and /help can
// stay silent about wording. Both name NEW sessions and neither claims anything
// about the running one — see this file's doc. Both fit inside the 80-column
// floor the golden tests pin, with room to spare: the status line is truncated
// to the terminal width (App.render), and a caveat that gets cut off leaves the
// unqualified overclaim behind.
const (
	yoloOnNote  = "Guardrails OFF (yolo) for NEW sessions; running sessions keep theirs."
	yoloOffNote = "Guardrails ON (ask) for new sessions; running sessions keep theirs."
)

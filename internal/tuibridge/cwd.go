package tuibridge

// cwd.go is the in-process half of jedwards1230/gofer#326: what a blank cwd on
// [Adapter.Resume] means, and what happens when the directory a session was
// recorded in no longer exists.
//
// The daemon-backed path answers both questions server-side (internal/daemon's
// resolveLoadCwd) and relays the second to the client as a typed signal
// (internal/daemonbridge's OnSessionCwdMissing). This file gives the daemonLESS
// backend the SAME two answers, because the wire is not what made them
// necessary: [supervisor.Supervisor.Resume] performs no cwd resolution of its
// own, so a blank cwd reaching it becomes runner.Options{Cwd: ""} and every
// cwd-scoped input a session has — the project config at <cwd>/.gofer/config.json,
// user commands under <cwd>/.gofer/commands, skills, and the bash/read/edit/
// write/grep/glob/ls tools' own working directory — silently roots at whatever
// directory the gofer PROCESS happens to be running in. That is the exact
// data-shaped failure the whole change exists to eliminate, and it is worse
// here than on the wire: nothing reports it at all.
//
// The branch table mirrors resolveLoadCwd's exactly, so the two backends cannot
// drift into meaning different things by the same wire value:
//
//	reqCwd    | recorded cwd            | outcome
//	----------|-------------------------|-----------------------------------------
//	non-blank | —                       | validated strictly; a bad path errors
//	blank     | session is LIVE         | no resolution at all — Resume ignores it
//	blank     | exists                  | resume there
//	blank     | recorded but gone       | signal + error; NEVER a substitute
//	blank     | none recorded           | "" through to the supervisor (unchanged)
//
// The last row is deliberately left as it was: there is no recorded directory
// being substituted away from, and in practice it only happens for a session id
// the supervisor does not know, whose Resume fails immediately afterwards.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// cwdMissingHandler is the registration slot behind [Adapter.OnSessionCwdMissing].
// It is a POINTER held by the Adapter value rather than a field on it: an
// Adapter is copied by value all over this package's API (New returns one, the
// TUI stores it in an interface), so a plain field could only ever be written on
// a copy nobody else holds. The atomic makes the registering goroutine's write
// visible to the Resume goroutine that reads it.
type cwdMissingHandler struct {
	fn atomic.Pointer[func(sessionID, cwd string)]
}

// OnSessionCwdMissing registers fn as this backend's handler for "the session's
// recorded working directory is gone", the same seam
// [daemonbridge.Supervisor.OnSessionCwdMissing] exposes and the TUI's
// cwdMissingNotifier asserts against — so an in-process TUI gets the same
// three-way prompt (pick a new directory / cancel / archive) a daemon-backed one
// does, instead of a session silently reopening in the TUI process's own
// directory. Passing nil clears the registration.
//
// fn runs on whichever goroutine called [Adapter.Resume] — never on a UI loop —
// and must return promptly: post a message and return.
func (a Adapter) OnSessionCwdMissing(fn func(sessionID, cwd string)) {
	if a.cwdMissing == nil {
		return
	}
	if fn == nil {
		a.cwdMissing.fn.Store(nil)
		return
	}
	a.cwdMissing.fn.Store(&fn)
}

// emitCwdMissing delivers one signal to the registered handler, if any.
func (a Adapter) emitCwdMissing(sessionID, cwd string) {
	if a.cwdMissing == nil {
		return
	}
	if fn := a.cwdMissing.fn.Load(); fn != nil {
		(*fn)(sessionID, cwd)
	}
}

// errCwdMissing is the error [Adapter.Resume] fails a blank-cwd resume with when
// the session's recorded directory no longer resolves. It exists so the failure
// is DISTINGUISHABLE from an ordinary resume error the same way the daemon's
// typed -32001 is — the TUI already learns about this case through the
// registered handler, so nothing decodes this today, but a caller that wants to
// branch can errors.As it rather than match the message.
type errCwdMissing struct {
	sessionID string
	cwd       string
	err       error
}

func (e *errCwdMissing) Error() string {
	return fmt.Sprintf("tuibridge: resume %s: session cwd %q, recorded when the session was created, is no longer available: %v",
		e.sessionID, e.cwd, e.err)
}

func (e *errCwdMissing) Unwrap() error { return e.err }

// resolveResumeCwd applies the table in this file's doc comment, returning the
// working directory to hand [supervisor.Supervisor.Resume] — or an error, never
// a substituted directory.
func (a Adapter) resolveResumeCwd(ctx context.Context, sessionID, reqCwd string) (string, error) {
	if strings.TrimSpace(reqCwd) != "" {
		cwd, err := validateResumeCwd(reqCwd)
		if err != nil {
			return "", fmt.Errorf("tuibridge: resume %s: %w", sessionID, err)
		}
		return cwd, nil
	}
	recorded, live, err := a.recordedCwd(ctx, sessionID)
	if err != nil {
		// Never degrade into the no-recorded-cwd branch below: a session that
		// HAS a recorded directory would then resume in the gofer process's own
		// one, silently, because a store read failed. "Never substitute a
		// directory" outranks "resume anyway".
		return "", fmt.Errorf("tuibridge: resume %s: %w", sessionID, err)
	}
	if live {
		// Already running: Resume returns the existing snapshot without building
		// a second runner, so the cwd it is handed is ignored. Resolving one
		// would only invent a way for attaching to a live session to FAIL — and
		// a prompt offering to re-init it elsewhere would state a directory the
		// early return then discards. Same reasoning as resolveLoadCwd's live
		// branch; keep the two together.
		return recorded, nil
	}
	if recorded == "" {
		return "", nil
	}
	cwd, err := validateResumeCwd(recorded)
	if err != nil {
		a.emitCwdMissing(sessionID, recorded)
		return "", &errCwdMissing{sessionID: sessionID, cwd: recorded, err: err}
	}
	return cwd, nil
}

// recordedCwd reads sessionID's recorded working directory, and whether it is
// live, off the supervisor's own [supervisor.Supervisor.List] — the identical
// seam internal/daemon's persistedCwd reads, which enriches every session (live
// with its live cwd, offline/archived from its journal). Reusing it rather than
// scanning the store again is the point: one enumeration, one definition of
// "where was this session recorded".
//
// A session the enumeration does not hold answers "", false, nil — the
// pre-existing no-recorded-cwd branch, where there is no recorded directory to
// be substituted away from. A FAILED enumeration is different in kind and is
// returned as an error: it means "this session's directory is unknown", not
// "this session has none", and collapsing the two would resume a session with a
// perfectly good recorded directory in the gofer process's own one because a
// disk read failed. (internal/daemon's persistedCwd does collapse them; that
// predates jedwards1230/gofer#326 and is not this seam's precedent to follow.)
func (a Adapter) recordedCwd(ctx context.Context, sessionID string) (cwd string, live bool, err error) {
	rows, err := a.sup.List(ctx)
	if err != nil {
		return "", false, fmt.Errorf("read the session list: %w", err)
	}
	for _, r := range rows {
		if r.ID == sessionID {
			return r.Cwd, r.Live, nil
		}
	}
	return "", false, nil
}

// validateResumeCwd is internal/daemon's resolveSessionCwd minus the
// empty-string default (this backend's callers have already branched on blank):
// a leading "~" expanded against this process's home, then absolute / exists /
// is-a-directory. Validating at all is what makes a directory the user PICKED in
// the re-init prompt fail loudly on a typo, rather than reaching runner.Options
// where a missing cwd shows up only as every tool call failing later.
func validateResumeCwd(raw string) (string, error) {
	cwd := strings.TrimSpace(raw)
	if cwd == "~" || strings.HasPrefix(cwd, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if cwd == "~" {
				cwd = home
			} else {
				cwd = filepath.Join(home, cwd[2:])
			}
		}
	}
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("session cwd %q must be an absolute path", raw)
	}
	fi, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("session cwd %q does not exist: %w", cwd, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("session cwd %q is not a directory", cwd)
	}
	return cwd, nil
}

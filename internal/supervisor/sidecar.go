package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// metaSuffix is the extension of a session's parent/agent sidecar, written
// alongside its `<id>.jsonl` journal as `<id>.meta.json`. It deliberately does
// NOT end in `.jsonl`, so the store's own project listing (which selects on that
// suffix — see [session.FileStore.List], the walk [Supervisor.List] drives) can
// never mistake a sidecar for a second session.
const metaSuffix = ".meta.json"

// sessionMeta is the durable subagent link for one session: which session
// spawned it, which agent identity it runs as, and how deep in the resulting
// tree it sits. It is gofer-native, not SDK: the SDK's journal has no concept of
// a session parent (supervision and roster stay in gofer per the SDK promotion
// test), so gofer records it itself — as an on-disk, greppable artifact next to
// the journal rather than as roster-only memory, per CLAUDE.md's "visible
// artifacts over hidden state".
//
// The zero value is a plain ROOT session, which is also what every session
// predating this file reads back as (see [readSessionMeta]): no sidecar means no
// parent, no agent, depth 0.
type sessionMeta struct {
	// ParentID is the id of the session that spawned this one; "" for a root
	// session.
	ParentID string `json:"parentId"`
	// Agent is this session's agent type/identity (e.g. "go-developer"),
	// forwarded to [runner.Options.Agent] so its tool-call events carry the
	// attribution field; "" for an un-attributed session.
	Agent string `json:"agent"`
	// Depth is 0 for a root session and parent.Depth+1 for a child.
	Depth int `json:"depth"`
}

// recordable reports whether m carries anything worth persisting. A plain root
// session has nothing to record, so it writes no sidecar at all — which is what
// keeps this feature invisible for every pre-existing use of the supervisor.
func (m sessionMeta) recordable() bool { return m.ParentID != "" || m.Agent != "" }

// sidecarPath is the sidecar file for id in the session directory dir (the
// directory its journal already lives in).
func sidecarPath(dir, id string) string { return filepath.Join(dir, id+metaSuffix) }

// writeSessionMeta persists m as id's sidecar in dir, atomically: it marshals to
// a temp file in the SAME directory (so the rename is same-filesystem, hence
// atomic) with mode 0600, then renames it over the final path — the same
// discipline [config.Save] uses for gofer's config file, so a reader never
// observes a half-written sidecar.
func writeSessionMeta(dir, id string, m sessionMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("supervisor: marshal session meta %s: %w", id, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("supervisor: mkdir %s: %w", dir, err)
	}
	path := sidecarPath(dir, id)
	tmp, err := os.CreateTemp(dir, "."+id+"-*"+metaSuffix+".tmp")
	if err != nil {
		return fmt.Errorf("supervisor: create temp session meta in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Clean up on any early return; after a successful Rename below the path no
	// longer exists, so this is a no-op on the happy path.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("supervisor: chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("supervisor: write %s: %w", tmpPath, err)
	}
	// fsync before the rename — the one place this diverges from [config.Save],
	// deliberately. Surviving a crash is this file's ENTIRE purpose: a rename is
	// atomic with respect to readers, but not with respect to power loss, so
	// without the sync a host that dies between rename and writeback can leave a
	// zero-length sidecar. readSessionMeta degrades that silently to "root
	// session" (by design — see its doc), so the failure would present as a
	// subagent quietly losing its parent rather than as an error anyone sees.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("supervisor: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("supervisor: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("supervisor: rename %s to %s: %w", tmpPath, path, err)
	}
	return nil
}

// readSessionMeta reads a sidecar. A MISSING or unparseable file yields the zero
// value and NO error, deliberately: every session written before this file
// existed has no sidecar, and a session must keep listing exactly as before
// rather than disappearing from the roster over a link it never had. The
// sidecar enriches a listing; it can never fail one.
func readSessionMeta(path string) sessionMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionMeta{}
	}
	var m sessionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return sessionMeta{}
	}
	return m
}

// DiskMeta reports the durable subagent link recorded for id under the session
// store rooted at root: the spawning session's id, the agent identity, and the
// depth. It is the ONE reader every offline-row builder must go through — the
// in-process [Supervisor.List] and the M6 router's own parallel List — so that
// "an offline child still shows its parent" holds on every deployment path, not
// just the in-process one.
//
// An unknown id, a session with no sidecar (every root session, and every
// session predating subagents), or an unreadable/corrupt sidecar all report the
// zero values and no error: the link ENRICHES a listing and can never fail one.
//
// It resolves id by scanning the project directories, exactly as the SDK's own
// store does ([session.FileStore] finds a journal by id the same way) — a
// session's directory is derived from the cwd it was CREATED with, which a later
// caller need not know. The scan is one ReadDir plus a Stat per project; a
// caller that already holds the session's directory should read the sidecar
// beside the journal directly instead (see [diskSessionInfo]).
func DiskMeta(root, id string) (parentID, agent string, depth int) {
	m, _ := lookupDiskSession(root, id)
	return m.ParentID, m.Agent, m.Depth
}

// lookupDiskSession finds id's session directory under root and returns its
// sidecar, reporting whether the session exists on disk at all.
//
// Existence is decided by the JOURNAL (`<id>.jsonl`), not by the sidecar: a root
// session — the common parent — writes no sidecar, so keying existence off the
// sidecar would make "spawn a child of an offline root session" impossible. A
// found session with no sidecar therefore returns the zero meta (depth 0), which
// is exactly right for a root.
//
// The scan is over `<root>/sessions/*` because a child's project slug is derived
// from ITS cwd and need not match its parent's, so the parent's directory is not
// knowable from the child's. It costs one ReadDir plus one Stat per project and
// runs only on the create-a-child path, never on a hot path.
func lookupDiskSession(root, id string) (sessionMeta, bool) {
	// A session id is a single path component by construction (the SDK rejects
	// anything else — see session.ErrInvalidID). Refusing one that isn't keeps a
	// client-supplied parent id from steering the Stat/ReadFile below out of the
	// store root.
	if id == "" || id == "." || filepath.Base(id) != id {
		return sessionMeta{}, false
	}
	sessionsDir := filepath.Join(root, "sessions")
	des, err := os.ReadDir(sessionsDir)
	if err != nil {
		return sessionMeta{}, false
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, de.Name())
		if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); err != nil {
			continue
		}
		return readSessionMeta(sidecarPath(dir, id)), true
	}
	return sessionMeta{}, false
}

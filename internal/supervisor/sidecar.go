package supervisor

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	// Archived records that the session was archived — dropped from the overview
	// roster while keeping its journal (architecture invariant #4). It lives in
	// the sidecar rather than the journal because the SDK journal has no
	// lifecycle entry type: an emitted session.archived event reaches connected
	// clients but is never written to the JSONL, so it would not survive a daemon
	// restart. Recording it here — a read-only overlay next to the journal, never
	// a mutation OF the journal — is what makes "archived stays off the roster
	// after a restart" durable. Zero value (false) is a non-archived session,
	// which is what every session predating this field reads back as.
	Archived bool `json:"archived,omitempty"`
	// ArchivedAt is when the session was archived; the zero time for a session
	// that never was. Diagnostic only — Archived is the load-bearing flag.
	ArchivedAt time.Time `json:"archivedAt,omitempty"`
	// Prompt is this session's composed system prompt provenance — which
	// files composed it, its content hash, and its length — recorded by
	// [RecordPrompt] for the CLI paths that build a session directly via
	// runner.New/Resume (see cmd/gofer's run/resume/exec and
	// internal/prompt.Compose). nil for a session predating this field or
	// one RecordPrompt was never called for (e.g. a daemon-created session —
	// see internal/prompt's package doc for what's wired so far).
	Prompt *promptMeta `json:"prompt,omitempty"`
	// Derived is the journal-derived roster metadata cached from the last
	// read of this session's journal — see [derivedMeta] and
	// [diskSessionInfo]. nil for a session predating this field or one no
	// offline listing has touched yet; a nil (or stale) Derived simply means
	// the next listing re-reads the journal, so it can never make a row wrong.
	Derived *derivedMeta `json:"derived,omitempty"`
}

// derivedMeta is the roster metadata [diskSessionInfo] otherwise recomputes by
// parsing a session's ENTIRE journal on every refresh — cwd, title, created and
// last-activity — cached beside the journal so a roster tick costs one small
// JSON read plus one Stat per offline session instead of a full JSONL parse
// (gofer#298: the TUI refreshes on a ~1s tick, so the full scan was being paid
// continuously, not just on the delete that made it noticeable).
//
// # Why the journal's size and mtime are a sound cache key
//
// A session journal is APPEND-ONLY — the SDK's [session.Journal] opens it
// O_APPEND and only ever writes whole entries, and gofer never rewrites or
// truncates one (architecture invariant #4: journals are never deleted). Every
// change to a journal is therefore an append of at least one non-empty line, so
// its SIZE strictly increases on every change. Size alone would very nearly do;
// mtime is carried alongside as a second, independent witness so that a journal
// replaced wholesale out-of-band (a restore from backup, a manual edit) is also
// caught unless it happens to match on both. The pair can only collide for a
// rewrite that preserves size AND mtime, which nothing in gofer can produce.
//
// The stat is taken BEFORE the journal is read, never after: a stat taken after
// the read could observe an append that landed mid-read and would then tag
// content-derived-from-the-old-bytes with the NEW size, pinning a stale cache
// entry until the next append. Stat-then-read can only err the other way — a
// cache entry tagged with a size older than the content it describes, which
// simply misses on the next listing and re-derives. Conservative in the safe
// direction.
type derivedMeta struct {
	Cwd     string    `json:"cwd,omitempty"`
	Title   string    `json:"title,omitempty"`
	Created time.Time `json:"created,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
	// JournalSize and JournalModNano are the journal's stat at the moment the
	// fields above were derived — the staleness key. Both are written even
	// when zero: a mismatch is what re-derives, so an absent value must not
	// read back as "matches anything".
	JournalSize    int64 `json:"journalSize"`
	JournalModNano int64 `json:"journalModNano"`
}

// fresh reports whether d still describes the journal fi stats — i.e. whether
// the cached fields can be served without re-reading the JSONL. A nil d (no
// sidecar, or one written by a gofer predating this field) is never fresh, so
// every legacy session falls back to the journal read and lists exactly as it
// did before.
func (d *derivedMeta) fresh(fi fs.FileInfo) bool {
	if d == nil || fi == nil {
		return false
	}
	return d.JournalSize == fi.Size() && d.JournalModNano == fi.ModTime().UnixNano()
}

// promptMeta is the on-disk shape of [PromptProvenance] — see [RecordPrompt].
type promptMeta struct {
	Files  []string `json:"files,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Bytes  int      `json:"bytes,omitempty"`
}

// recordable reports whether m carries anything worth persisting. A plain root
// session has nothing to record, so it writes no sidecar at all — which is what
// keeps this feature invisible for every pre-existing use of the supervisor. An
// archived session, one with recorded prompt provenance, or one with cached
// roster metadata is always recordable: each is the whole point of the sidecar
// for a plain root session that has no parent/agent link.
func (m sessionMeta) recordable() bool {
	return m.ParentID != "" || m.Agent != "" || m.Archived || m.Prompt != nil || m.Derived != nil
}

// sidecarPath is the sidecar file for id in the session directory dir (the
// directory its journal already lives in).
func sidecarPath(dir, id string) string { return filepath.Join(dir, id+metaSuffix) }

// sidecarMu serializes every read-modify-write of a sidecar in this process.
//
// [atomicWriteFile] already makes a single write atomic for READERS, but the
// sidecar has several independent RMW writers — the archive marker, prompt
// provenance, and (on every roster refresh, for every offline session whose
// journal grew) the derived-metadata cache. Without this lock, an archive
// landing between a concurrent refresh's read and its write-back would be
// silently overwritten with Archived=false, resurfacing an archived session on
// the overview. The lock is process-wide rather than per-path because sidecar
// writes are rare relative to reads and never held across a journal read.
//
// It does NOT coordinate across processes (the M6 router writes the same files
// from its own process); the pre-existing exposure there is unchanged, and the
// derived cache in particular degrades to a re-derive rather than to a wrong
// row.
var sidecarMu sync.Mutex

// writeSessionMeta persists m as id's sidecar in dir, atomically — see
// [atomicWriteFile]. Callers replacing a whole sidecar go through here; callers
// mutating one field of an existing sidecar must use [updateSessionMeta] so the
// read and the write are one critical section.
func writeSessionMeta(dir, id string, m sessionMeta) error {
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	return writeSessionMetaLocked(dir, id, m)
}

// writeSessionMetaLocked is [writeSessionMeta]'s body; the caller holds
// [sidecarMu].
func writeSessionMetaLocked(dir, id string, m sessionMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("supervisor: marshal session meta %s: %w", id, err)
	}
	return atomicWriteFile(dir, "."+id+"-*"+metaSuffix+".tmp", sidecarPath(dir, id), data)
}

// updateSessionMeta read-modify-writes id's sidecar under dir under
// [sidecarMu], so a concurrent update of a DIFFERENT field cannot be lost.
// mutate reports whether it changed anything; false means no write at all (no
// file churn for a no-op). A mutation that leaves nothing worth recording (see
// [sessionMeta.recordable]) removes the sidecar rather than leaving an all-zero
// stub — a missing sidecar reads back as the same zero value.
func updateSessionMeta(dir, id string, mutate func(*sessionMeta) bool) error {
	sidecarMu.Lock()
	defer sidecarMu.Unlock()

	meta := readSessionMeta(sidecarPath(dir, id))
	if !mutate(&meta) {
		return nil
	}
	if !meta.recordable() {
		if err := os.Remove(sidecarPath(dir, id)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("supervisor: remove session meta %s: %w", id, err)
		}
		return nil
	}
	return writeSessionMetaLocked(dir, id, meta)
}

// atomicWriteFile writes data to path, atomically: a temp file in the SAME
// directory as path (so the rename is same-filesystem, hence atomic) at mode
// 0600, fsynced before the rename, then renamed over path — the same
// discipline [config.Save] uses for gofer's config file, with one deliberate
// divergence: the fsync. Surviving a crash is the whole point of a durability
// artifact like a session sidecar or its prompt text, and a rename alone is
// atomic with respect to READERS but not with respect to power loss — without
// the sync, a host that dies between rename and writeback can leave a
// truncated file behind. Every caller here degrades a missing/corrupt file
// silently rather than erroring (see readSessionMeta, [PromptText]), so that
// failure would present as quietly losing durable data rather than as an
// error anyone sees. tmpGlob is the [os.CreateTemp] pattern (must contain
// exactly one "*").
func atomicWriteFile(dir, tmpGlob, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("supervisor: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, tmpGlob)
	if err != nil {
		return fmt.Errorf("supervisor: create temp file in %s: %w", dir, err)
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

// diskSessionDir returns the directory holding id's journal under the store
// rooted at root, reporting whether id exists on disk at all. It is the
// dir-returning twin of [lookupDiskSession] (existence is decided by the
// `<id>.jsonl` journal, never the sidecar), for a caller that must WRITE the
// sidecar of an offline session and so needs its directory — chiefly
// [Supervisor.Archive] marking a session archived after a restart, when there is
// no live [managed] to read the journal path from.
func diskSessionDir(root, id string) (dir string, ok bool) {
	if id == "" || id == "." || filepath.Base(id) != id {
		return "", false
	}
	sessionsDir := filepath.Join(root, "sessions")
	des, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", false
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		d := filepath.Join(sessionsDir, de.Name())
		if _, err := os.Stat(filepath.Join(d, id+".jsonl")); err != nil {
			continue
		}
		return d, true
	}
	return "", false
}

// setArchived records (or clears) id's archive marker in its sidecar under dir,
// read-modify-write so the subagent link (ParentID/Agent/Depth) an archived
// session may also carry is preserved. Setting it is what makes "archived stays
// off the roster after a restart" durable; clearing it is how a resumed session
// returns to the overview for good (see [Supervisor.Resume]).
//
// It never touches the journal — only the `.meta.json` sidecar — which is what
// keeps the rebuild-from-journals guarantee (journals are read-only over this
// change) intact. When clearing leaves a plain root session with nothing left to
// record, the now-empty sidecar is removed rather than left as a stub.
func setArchived(dir, id string, archived bool, now time.Time) error {
	return updateSessionMeta(dir, id, func(meta *sessionMeta) bool {
		if meta.Archived == archived {
			return false // already in the desired state — no write, no churn
		}
		meta.Archived = archived
		if archived {
			meta.ArchivedAt = now
		} else {
			meta.ArchivedAt = time.Time{}
		}
		return true
	})
}

// cacheDerived records d as id's journal-derived roster metadata in its sidecar
// under dir, preserving every other field (subagent link, archive marker,
// prompt provenance) — see [derivedMeta] for why this is safe to serve back and
// [diskSessionInfo] for who calls it.
//
// It is BEST-EFFORT by contract: the caller has already built a correct row
// from the journal it just read, so a failed write costs a re-derive on the next
// refresh and nothing else. That matters on a read-only store or a directory the
// listing process cannot write, where failing the listing instead would take the
// whole roster down to save a cache.
//
// It writes only the `.meta.json` sidecar, never the JSONL — the listing path
// stays read-only over the journals.
func cacheDerived(dir, id string, d derivedMeta) error {
	return updateSessionMeta(dir, id, func(meta *sessionMeta) bool {
		if meta.Derived != nil && *meta.Derived == d {
			return false // already current — no write, no churn
		}
		meta.Derived = &d
		return true
	})
}

// DiskArchived reports whether id was archived, read from its sidecar under the
// store rooted at root. Like [DiskMeta] it is the reader an offline-row builder
// goes through so the flag holds on every deployment path (the in-process
// [Supervisor.List] and the M6 router's own parallel List). An unknown id, a
// session with no sidecar, or an unreadable one all report false — archived is
// an overlay on a listing and can never fail one.
func DiskArchived(root, id string) bool {
	m, _ := lookupDiskSession(root, id)
	return m.Archived
}

// SidecarInfo is the durable per-session metadata a `.meta.json` sidecar carries
// beside its journal: the subagent link (which session spawned it, its agent
// identity, its depth) and the archive marker. It is the exported projection of
// the unexported [sessionMeta] for cross-package offline-row builders (the M6
// router).
type SidecarInfo struct {
	ParentID string
	Agent    string
	Depth    int
	Archived bool
	// PromptFiles/PromptSHA256/PromptBytes are [RecordPrompt]'s payload read
	// back — see [PromptProvenance]. PromptFiles is nil when no prompt was
	// ever recorded for this session (mirrors every other zero-value-means-
	// absent field here).
	PromptFiles  []string
	PromptSHA256 string
	PromptBytes  int
}

// ReadSidecar reads id's sidecar from dir — the directory that already holds its
// journal — returning the zero value for a missing or unreadable one (the same
// degrade-to-root-session contract [readSessionMeta] has). It is the
// by-directory reader an offline-row builder that ALREADY knows the session's
// directory should use, folding the subagent link and the archive marker into
// one read and avoiding [DiskMeta]/[DiskArchived]'s per-session project scan.
func ReadSidecar(dir, id string) SidecarInfo {
	m := readSessionMeta(sidecarPath(dir, id))
	info := SidecarInfo{ParentID: m.ParentID, Agent: m.Agent, Depth: m.Depth, Archived: m.Archived}
	if m.Prompt != nil {
		info.PromptFiles = m.Prompt.Files
		info.PromptSHA256 = m.Prompt.SHA256
		info.PromptBytes = m.Prompt.Bytes
	}
	return info
}

// SetArchivedOnDisk records (archived=true) or clears (false) id's durable
// archive marker in its sidecar under the store rooted at root, resolving id's
// directory on disk first. It reports whether id was found on disk at all (a
// caller archiving a genuinely-unknown id decides what that means — the M6
// router treats it as a no-op offline archive, the in-process supervisor as
// [ErrNotLive]). Journal-safe: it writes only the `.meta.json` sidecar, never
// the JSONL. It is the offline-archive path for a caller with no live session to
// stop — chiefly the M6 router, whose offline sessions have no worker to forward
// a gofer/archive to.
func SetArchivedOnDisk(root, id string, archived bool, now time.Time) (found bool, err error) {
	dir, ok := diskSessionDir(root, id)
	if !ok {
		return false, nil
	}
	return true, setArchived(dir, id, archived, now)
}

// promptTextSuffix is the extension of a session's composed system prompt,
// written alongside its `<id>.jsonl` journal and `<id>.meta.json` sidecar as
// `<id>.system.md` — see [RecordPrompt].
const promptTextSuffix = ".system.md"

// promptTextPath is the composed-prompt file for id in the session directory
// dir (the directory its journal already lives in).
func promptTextPath(dir, id string) string { return filepath.Join(dir, id+promptTextSuffix) }

// PromptProvenance is a session's composed system prompt's durable record:
// which sources composed it (in the order [prompt.Compose] resolved them,
// post de-dup — see its [prompt.Composed]), a hex SHA-256 digest of the
// composed text, and its length in bytes. It is [RecordPrompt]'s input.
type PromptProvenance struct {
	Files  []string
	SHA256 string
	Bytes  int
}

// RecordPrompt persists prov into id's `.meta.json` sidecar
// (read-modify-write, preserving any existing subagent link or archive
// marker already there) and writes text — the composed system prompt
// verbatim — as `<id>.system.md` beside the journal at journalPath.
//
// It exists because cmd/gofer's run/resume/exec build a session directly via
// runner.New/Resume, bypassing this package's own Create/Resume (and the
// subagent-link sidecar write those already do) entirely — without this,
// nothing records which files actually composed a session's prompt, so a
// resumed session's original prompt was undiscoverable once composed (see
// internal/prompt's package doc and CLAUDE.md's "visible artifacts over
// hidden state"). It does NOT change what a resume runs WITH: resume still
// recomposes fresh from current config on every call (a project's AGENTS.md
// legitimately changes between sessions), so this is the audit trail of what
// a session actually ran with THIS time, not a frozen replay input.
//
// The session this describes is already running by the time a caller reaches
// this point, so a write failure here is a best-effort diagnostic loss, not a
// reason to tear the session down — RecordPrompt returns the error for the
// caller to log rather than to fail the run on.
func RecordPrompt(id, journalPath string, prov PromptProvenance, text string) error {
	dir := filepath.Dir(journalPath)
	err := updateSessionMeta(dir, id, func(meta *sessionMeta) bool {
		meta.Prompt = &promptMeta{Files: prov.Files, SHA256: prov.SHA256, Bytes: prov.Bytes}
		return true
	})
	if err != nil {
		return fmt.Errorf("supervisor: record prompt provenance for %s: %w", id, err)
	}
	if err := atomicWriteFile(dir, "."+id+"-*"+promptTextSuffix+".tmp", promptTextPath(dir, id), []byte(text)); err != nil {
		return fmt.Errorf("supervisor: write prompt text for %s: %w", id, err)
	}
	return nil
}

// PromptText reads id's composed system prompt back from its
// `<id>.system.md` file under dir — the directory that already holds its
// journal. It exists chiefly for tests verifying [RecordPrompt]'s round
// trip; nothing in gofer reads it back on a session-restore path (see
// [RecordPrompt]'s doc).
func PromptText(dir, id string) (string, error) {
	data, err := os.ReadFile(promptTextPath(dir, id)) //nolint:gosec // dir/id are the caller's own resolved session directory and id, not user input
	if err != nil {
		return "", err
	}
	return string(data), nil
}

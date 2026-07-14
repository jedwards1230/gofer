package sandbox

import "fmt"

// seatbeltProfile generates the SBPL (Sandbox Profile Language) policy for a
// command confined to workdir: deny-by-default, read access to the base
// system so ordinary interpreters/toolchains resolve, read+write inside the
// session workdir plus the per-user temp dir, and network access denied
// outright.
//
// It is a pure function of workdir alone — it never reads os.Environ or any
// other process state — so the generated profile can never embed a secret
// that happens to be sitting in the environment. Keep it that way: this is a
// security property the tests assert on (see profile_seatbelt_test.go).
func seatbeltProfile(workdir string) string {
	return fmt.Sprintf(`(version 1)
(deny default)
(debug deny)

; Process control needed to actually run the wrapped command.
(allow process-fork)
(allow process-exec)
(allow signal (target self))
(allow sysctl-read)
(allow mach-lookup)

; Read access to the base system so interpreters/toolchains resolve.
(allow file-read*
    (subpath "/usr")
    (subpath "/bin")
    (subpath "/sbin")
    (subpath "/System")
    (subpath "/Library")
    (subpath "/private/etc")
    (subpath "/private/var/db/timezone")
    (subpath "/private/var/select")
    (subpath "/dev")
    (subpath "/opt")
    (literal "/"))

; getcwd(3) and path resolution must stat/traverse the workdir's ancestor
; directories up to root; grant metadata-only access so the shell's startup
; getcwd and any pipe/subprocess setup resolve cleanly. file-read-metadata
; permits stat/lstat/access only — NOT file contents (file-read-data) and NOT
; writes (verified: stat of an out-of-tree file succeeds, cat of it is
; denied). It widens metadata visibility (existence/size/mode) tree-wide but
; never exposes file contents, so no secret data can be read through it.
(allow file-read-metadata
    (subpath "/"))

; Read+write confined to the session workdir only.
(allow file-read* file-write*
    (subpath %q))

; Many tools (mktemp, compilers, $TMPDIR consumers) write scratch files to the
; per-user temp dir rather than the workdir, so grant read+write there. This is
; /private/var/folders — the macOS per-user DARWIN_USER_TEMP_DIR that $TMPDIR
; points at and that mktemp(1) resolves to even when $TMPDIR is unset (verified,
; so it also covers headless/daemon runs with no $TMPDIR). It is per-user
; scoped, unlike the world-writable shared /tmp, which is deliberately NOT
; granted: excluding it keeps the sandbox's write set confined to per-user
; scratch plus the workdir. The profile stays a pure function of workdir and
; never reads $TMPDIR from the environment, so this is the static well-known
; root.
(allow file-read* file-write*
    (subpath "/private/var/folders"))

; A handful of always-safe device nodes tools expect to write to.
(allow file-write-data
    (literal "/dev/null")
    (literal "/dev/stdout")
    (literal "/dev/stderr")
    (literal "/dev/tty"))

; No network access whatsoever.
(deny network*)
`, workdir)
}

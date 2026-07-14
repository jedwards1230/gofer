package sandbox

import "fmt"

// seatbeltProfile generates the SBPL (Sandbox Profile Language) policy for a
// command confined to workdir: deny-by-default, read access to the base
// system so ordinary interpreters/toolchains resolve, read+write ONLY inside
// the session workdir, and network access denied outright.
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
    (subpath "/dev")
    (subpath "/opt")
    (literal "/"))

; Read+write confined to the session workdir only.
(allow file-read* file-write*
    (subpath %q))

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

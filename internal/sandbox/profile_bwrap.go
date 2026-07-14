package sandbox

// bwrapArgv builds the bwrap invocation that runs command confined to
// workdir: the whole filesystem is bind-mounted read-only except workdir
// (bound read+write in place), fresh /dev and /proc are provided, and
// --unshare-net drops network access entirely for the child (and anything it
// forks) — the same "no network" guarantee the seatbelt profile's
// `(deny network*)` gives on macOS. --die-with-parent keeps a stray bwrap
// process from outliving the tool call that spawned it.
//
// It is a pure function of command and workdir alone — it never reads
// os.Environ or any other process state — so the generated argv can never
// embed a secret that happens to be sitting in the environment. Keep it that
// way: this is a security property the tests assert on (see
// profile_bwrap_test.go).
//
// Syscall-level filtering (seccomp) beyond the namespace/mount isolation
// above is not wired up yet — bwrap supports a --seccomp <fd> flag taking a
// pre-compiled BPF program, which needs a build-time filter compiler this
// package does not carry. The namespace unshare + read-only root already
// covers the M3 threat model (network egress, host filesystem mutation
// outside workdir); a seccomp filter is a follow-up hardening step, not a
// blocker for containment to work.
func bwrapArgv(command, workdir string) []string {
	return []string{
		"bwrap",
		"--ro-bind", "/", "/",
		"--bind", workdir, workdir,
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-net",
		"--die-with-parent",
		"/bin/sh", "-c", command,
	}
}

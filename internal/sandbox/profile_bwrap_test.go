package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestBwrapArgv_Shape(t *testing.T) {
	argv := bwrapArgv("echo hi", "/tmp/session-work")

	want := []string{
		"bwrap",
		"--ro-bind", "/", "/",
		"--bind", "/tmp/session-work", "/tmp/session-work",
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-net",
		"--die-with-parent",
		"/bin/sh", "-c", "echo hi",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("bwrapArgv() = %v, want %v", argv, want)
	}
}

func TestBwrapArgv_NetworkUnshared(t *testing.T) {
	argv := bwrapArgv("echo hi", "/tmp/session-work")
	if !slices.Contains(argv, "--unshare-net") {
		t.Errorf("argv missing --unshare-net: %v", argv)
	}
}

func TestBwrapArgv_RootReadOnlyWorkdirBound(t *testing.T) {
	argv := bwrapArgv("echo hi", "/tmp/session-work")

	roIdx := slices.Index(argv, "--ro-bind")
	if roIdx == -1 || roIdx+2 >= len(argv) || argv[roIdx+1] != "/" || argv[roIdx+2] != "/" {
		t.Errorf("argv missing --ro-bind / /: %v", argv)
	}

	bindIdx := slices.Index(argv, "--bind")
	if bindIdx == -1 || bindIdx+2 >= len(argv) ||
		argv[bindIdx+1] != "/tmp/session-work" || argv[bindIdx+2] != "/tmp/session-work" {
		t.Errorf("argv missing --bind <workdir> <workdir>: %v", argv)
	}
}

// TestBwrapArgv_NoSecretLeak asserts the argv generator never embeds
// process-environment content: it is a pure function of command and workdir,
// so a secret sitting in the environment must never appear in the generated
// argv.
func TestBwrapArgv_NoSecretLeak(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET", "super-secret-token-do-not-leak")

	argv := bwrapArgv("echo hi", "/tmp/session-work")

	for _, a := range argv {
		if strings.Contains(a, "super-secret-token-do-not-leak") {
			t.Fatalf("argv leaked env secret value: %v", argv)
		}
		if strings.Contains(a, "GOFER_TEST_SECRET") {
			t.Fatalf("argv leaked env var name: %v", argv)
		}
	}
}

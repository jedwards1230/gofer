package main

// service_model_test.go covers `gofer daemon install -m` — the escape hatch
// issue #147 was missing. With more than one provider logged in, `gofer
// daemon` refuses to start without a model, and before this flag existed
// there was no way to put one into the generated unit: the installed service
// respawn-looped, its error visible only in a log file, with hand-editing the
// plist/unit or logging a provider out the only ways out.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonInstallWritesModelIntoUnit proves -m reaches the installed unit's
// daemon argv as --model, which is what makes the service start at all in the
// ambiguous-provider case. Asserted on the rendered unit body, not merely on
// the serviceConfig, so a renderer that dropped the extra args would fail too.
func TestDaemonInstallWritesModelIntoUnit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	if err := runDaemonInstall(context.Background(), []string{"-m", "gpt-5"}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonInstall: %v (stderr=%q)", err, errBuf.String())
	}
	body, err := os.ReadFile(fake.path)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !bytes.Contains(body, []byte("--model gpt-5")) {
		t.Errorf("unit file missing --model gpt-5:\n%s", body)
	}
	assertNoToken(t, body)
}

// TestDaemonInstallModelAlias proves --model is accepted too: the flag is
// spelled -m to match `gofer run` and --model to match `gofer daemon`, the
// flag it is written into, and an operator should not have to know which.
func TestDaemonInstallModelAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	if err := runDaemonInstall(context.Background(), []string{"--model", "gpt-5"}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonInstall: %v (stderr=%q)", err, errBuf.String())
	}
	body, err := os.ReadFile(fake.path)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !bytes.Contains(body, []byte("--model gpt-5")) {
		t.Errorf("unit file missing --model gpt-5:\n%s", body)
	}
}

// TestDaemonInstallNoModelLeavesArgvUnchanged pins the opt-in: without -m the
// argv is byte-for-byte what it was before the flag existed, so the daemon
// still resolves its own default at startup.
func TestDaemonInstallNoModelLeavesArgvUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	if err := runDaemonInstall(context.Background(), nil, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonInstall: %v (stderr=%q)", err, errBuf.String())
	}
	body, err := os.ReadFile(fake.path)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if bytes.Contains(body, []byte("--model")) {
		t.Errorf("unit file carries --model with no -m given:\n%s", body)
	}
}

// TestDaemonInstallRejectsUnknownModel proves the id is validated at INSTALL
// time. Writing an unknown model into the unit would reproduce the original
// bug in a new costume: a service that loads, then exits at startup with the
// reason buried in a log file. The check must also leave no unit behind.
func TestDaemonInstallRejectsUnknownModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	err := runDaemonInstall(context.Background(), []string{"-m", "not-a-real-model"}, &out, &errBuf)
	if err == nil {
		t.Fatal("runDaemonInstall with an unknown model: got nil error, want a usage error")
	}
	var uerr *usageError
	if !errors.As(err, &uerr) {
		t.Errorf("err = %T (%v), want *usageError (exit 2)", err, err)
	}
	if !strings.Contains(err.Error(), "not-a-real-model") {
		t.Errorf("err = %q, want it to name the rejected model", err)
	}
	if _, statErr := os.Stat(fake.path); !os.IsNotExist(statErr) {
		t.Errorf("a unit file was written despite the rejected model (err=%v)", statErr)
	}
	if fake.loadCalls != 0 {
		t.Errorf("load called %d times on a rejected install, want 0", fake.loadCalls)
	}
}

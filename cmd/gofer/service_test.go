package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGoldens regenerates the *.golden fixtures instead of asserting against
// them. Run `go test ./cmd/gofer -run TestRender -update` after an intentional
// renderer change.
var updateGoldens = flag.Bool("update", false, "regenerate testdata/*.golden files")

// goldenServiceConfig is the fixed, stable config the renderer goldens pin to.
// Deliberately token-free (serviceConfig carries no token field) so the goldens
// can double as the token-absence assertion below.
func goldenServiceConfig(label string) serviceConfig {
	const (
		exec = "/usr/local/bin/gofer"
		root = "/home/u/.gofer"
		addr = "127.0.0.1:7333"
	)
	return serviceConfig{
		Label:      label,
		ExecPath:   exec,
		ListenAddr: addr,
		Root:       root,
		Args:       []string{"daemon", "--listen", addr, "--root", root},
	}
}

func TestRenderLaunchdPlistGolden(t *testing.T) {
	got := renderLaunchdPlist(goldenServiceConfig("com.github.jedwards1230.gofer"))
	assertGolden(t, "launchd.plist.golden", got)
	assertNoToken(t, got)
}

func TestRenderSystemdUnitGolden(t *testing.T) {
	got := renderSystemdUnit(goldenServiceConfig("gofer.service"))
	assertGolden(t, "systemd.service.golden", got)
	assertNoToken(t, got)
}

// assertGolden compares got against testdata/<name>, regenerating it under
// -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("rendered output does not match %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// assertNoToken guards the token-safety invariant at the renderer boundary: no
// unit-file body may ever carry a bearer token or the GOFER_TOKEN key.
func assertNoToken(t *testing.T, got []byte) {
	t.Helper()
	for _, needle := range []string{"GOFER_TOKEN", "--token", "s3cr3t-token"} {
		if bytes.Contains(got, []byte(needle)) {
			t.Errorf("rendered unit file must never contain %q, got:\n%s", needle, got)
		}
	}
}

// TestShouldPromptInstall exhaustively covers the pure gating predicate: it is
// true ONLY in the all-clear interactive case, false whenever any one input
// disqualifies the prompt.
func TestShouldPromptInstall(t *testing.T) {
	tests := []struct {
		name                                                   string
		stdinTTY, stdoutTTY, ciEnv, installed, reachable, want bool
	}{
		{"all clear interactive", true, true, false, false, false, true},
		{"piped stdin", false, true, false, false, false, false},
		{"non-tty stdout", true, false, false, false, false, false},
		{"in CI", true, true, true, false, false, false},
		{"service already installed", true, true, false, true, false, false},
		{"daemon already reachable", true, true, false, false, true, false},
		{"nothing interactive at all", false, false, true, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPromptInstall(tt.stdinTTY, tt.stdoutTTY, tt.ciEnv, tt.installed, tt.reachable)
			if got != tt.want {
				t.Errorf("shouldPromptInstall(%v,%v,%v,%v,%v) = %v, want %v",
					tt.stdinTTY, tt.stdoutTTY, tt.ciEnv, tt.installed, tt.reachable, got, tt.want)
			}
		})
	}
}

// fakeServiceManager records load/unload calls and writes the unit to a temp
// path instead of shelling out to launchctl/systemctl.
type fakeServiceManager struct {
	path        string
	body        []byte
	installed   bool
	isRunning   bool
	loadCalls   int
	unloadCalls int
	loadErr     error

	// unloadDidWork is what unload reports as its "anything actually unloaded"
	// bool; unloadErr is the error it returns. Both default to the zero value
	// (nothing unloaded, no error), which is the honest answer for a fake whose
	// service was never running.
	unloadDidWork bool
	unloadErr     error

	// stopCalls/startCalls count the stopService/startService lever `gofer daemon
	// stop|restart` pulls for a service-managed daemon; stopDidWork is what
	// stopService reports.
	stopCalls   int
	startCalls  int
	stopDidWork bool
	stopErr     error

	// reloadCalls counts reloadAfterRemove invocations; fileGoneAtReload records
	// whether the unit file was already removed by the time reloadAfterRemove
	// ran — proof the command layer reloads AFTER os.Remove, not before.
	reloadCalls      int
	fileGoneAtReload bool
}

func (f *fakeServiceManager) label() string             { return "gofer.test" }
func (f *fakeServiceManager) unitPath() (string, error) { return f.path, nil }
func (f *fakeServiceManager) render(cfg serviceConfig) []byte {
	f.body = renderSystemdUnit(cfg)
	return f.body
}

func (f *fakeServiceManager) isInstalled() (bool, error) {
	if _, err := os.Stat(f.path); err == nil {
		return true, nil
	}
	return f.installed, nil
}

func (f *fakeServiceManager) load(_ context.Context, _ string) error {
	f.loadCalls++
	return f.loadErr
}

func (f *fakeServiceManager) unload(_ context.Context, _ string) (bool, error) {
	f.unloadCalls++
	return f.unloadDidWork, f.unloadErr
}

func (f *fakeServiceManager) stopService(_ context.Context, _ string) (bool, error) {
	f.stopCalls++
	if f.stopErr != nil {
		return false, f.stopErr
	}
	if f.stopDidWork {
		f.isRunning = false
	}
	return f.stopDidWork, nil
}

func (f *fakeServiceManager) startService(_ context.Context, _ string) error {
	f.startCalls++
	return nil
}

func (f *fakeServiceManager) reloadAfterRemove(_ context.Context) error {
	f.reloadCalls++
	if _, err := os.Stat(f.path); os.IsNotExist(err) {
		f.fileGoneAtReload = true
	}
	return nil
}

func (f *fakeServiceManager) running(_ context.Context) (bool, error) { return f.isRunning, nil }

// withFakeManager swaps the newServiceManager seam to return fake for the test
// duration and restores it after.
func withFakeManager(t *testing.T, fake *fakeServiceManager) {
	t.Helper()
	prev := newServiceManager
	newServiceManager = func() serviceManager { return fake }
	t.Cleanup(func() { newServiceManager = prev })
}

// forcePromptGate overrides the promptGate seam to a fixed answer so the
// interactive branch is exercisable without a real TTY or network probe.
func forcePromptGate(t *testing.T, want bool) {
	t.Helper()
	prev := promptGate
	promptGate = func(context.Context, io.Writer, serviceManager) bool { return want }
	t.Cleanup(func() { promptGate = prev })
}

// TestMaybePromptNoOpWhenNonInteractive asserts the prompt is a complete no-op
// — zero output on stdout/stderr and no stdin consumed — when the gate is
// false (the real gate: a *bytes.Buffer is never a TTY).
func TestMaybePromptNoOpWhenNonInteractive(t *testing.T) {
	hermeticDaemonEnv(t)
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	stdin := strings.NewReader("y\n") // must be left untouched
	maybePromptDaemonServiceInstall(context.Background(), stdin, &out, &errBuf)

	if out.Len() != 0 || errBuf.Len() != 0 {
		t.Errorf("non-interactive prompt produced output: stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	// The whole stdin must remain unread.
	rest, _ := readLine(stdin)
	if !strings.Contains(rest, "y") {
		t.Errorf("stdin was consumed by the no-op prompt; remaining=%q", rest)
	}
	if fake.loadCalls != 0 {
		t.Errorf("no-op prompt called load %d times, want 0", fake.loadCalls)
	}
}

// TestMaybePromptAcceptInstalls drives the accept path with a forced-interactive
// gate: answering "y" writes the unit and calls load.
func TestMaybePromptAcceptInstalls(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	t.Setenv("HOME", root) // resolveSelfExec + ResolveRoot land here
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)
	forcePromptGate(t, true)

	var out, errBuf bytes.Buffer
	maybePromptDaemonServiceInstall(context.Background(), strings.NewReader("y\n"), &out, &errBuf)

	if !strings.Contains(out.String(), "Install one so it starts on login?") {
		t.Errorf("accept path did not print the prompt; stdout=%q", out.String())
	}
	if fake.loadCalls != 1 {
		t.Fatalf("accept path called load %d times, want 1 (stderr=%q)", fake.loadCalls, errBuf.String())
	}
	if _, err := os.Stat(fake.path); err != nil {
		t.Errorf("accept path did not write the unit file: %v", err)
	}
}

// TestMaybePromptDeclineDoesNotInstall covers the decline + EOF branches: no
// load, no unit file.
func TestMaybePromptDeclineDoesNotInstall(t *testing.T) {
	for _, answer := range []string{"n\n", "", "\n"} {
		t.Run("answer="+strings.TrimSpace(answer), func(t *testing.T) {
			hermeticDaemonEnv(t)
			fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
			withFakeManager(t, fake)
			forcePromptGate(t, true)

			var out, errBuf bytes.Buffer
			maybePromptDaemonServiceInstall(context.Background(), strings.NewReader(answer), &out, &errBuf)

			if fake.loadCalls != 0 {
				t.Errorf("decline path called load %d times, want 0", fake.loadCalls)
			}
			if _, err := os.Stat(fake.path); !os.IsNotExist(err) {
				t.Errorf("decline path wrote a unit file (err=%v), want none", err)
			}
		})
	}
}

// TestRunDaemonInstallViaSeam covers the install command through the fake:
// the unit file is written to the fake's path and load is called once.
func TestRunDaemonInstallViaSeam(t *testing.T) {
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
	if !bytes.Contains(body, []byte("ExecStart=")) {
		t.Errorf("unit file missing ExecStart: %s", body)
	}
	assertNoToken(t, body)
	if fake.loadCalls != 1 {
		t.Errorf("install called load %d times, want 1", fake.loadCalls)
	}
}

// TestRunDaemonUninstallViaSeam covers uninstall through the fake: the unit
// file is removed and unload is called once.
func TestRunDaemonUninstallViaSeam(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	// unloadDidWork: the service WAS loaded, so this covers the real-uninstall
	// path (see TestRunDaemonUninstallReportsNothingUnloaded for the other one).
	fake := &fakeServiceManager{path: path, unloadDidWork: true}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	if err := runDaemonUninstall(context.Background(), nil, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonUninstall: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "Uninstalled") {
		t.Errorf("stdout = %q, want the success message when a loaded service was unloaded", out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("uninstall left the unit file behind (err=%v)", err)
	}
	if fake.unloadCalls != 1 {
		t.Errorf("uninstall called unload %d times, want 1", fake.unloadCalls)
	}
	// The manager reload must run exactly once, and only AFTER the unit file was
	// removed (systemd's daemon-reload would forget nothing if it ran first).
	if fake.reloadCalls != 1 {
		t.Errorf("uninstall called reloadAfterRemove %d times, want 1", fake.reloadCalls)
	}
	if !fake.fileGoneAtReload {
		t.Errorf("reloadAfterRemove ran while the unit file still existed; it must run after os.Remove")
	}
}

// TestRunDaemonInstallNonLoopbackRequiresToken asserts the install command
// enforces ValidateListen: a non-loopback --listen with no token is refused
// before any unit file is written.
func TestRunDaemonInstallNonLoopbackRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GOFER_TOKEN", "")
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	err := runDaemonInstall(context.Background(), []string{"--listen", "192.168.1.50:7333"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "refusing to bind") {
		t.Fatalf("runDaemonInstall(non-loopback, no token) err = %v, want a refusing-to-bind error", err)
	}
	if _, statErr := os.Stat(fake.path); !os.IsNotExist(statErr) {
		t.Errorf("a refused install must not write a unit file (err=%v)", statErr)
	}
	if fake.loadCalls != 0 {
		t.Errorf("a refused install must not call load")
	}
}

// TestRunDaemonInstallNonLoopbackWritesTokenEnvFile asserts the token-safety
// design: a non-loopback install writes the token ONLY to the 0600
// <root>/daemon.env file, never into the unit file.
func TestRunDaemonInstallNonLoopbackWritesTokenEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	fake := &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
	withFakeManager(t, fake)

	const tok = "s3cr3t-token"
	var out, errBuf bytes.Buffer
	err := runDaemonInstall(context.Background(),
		[]string{"--listen", "192.168.1.50:7333", "--token", tok, "--root", root}, &out, &errBuf)
	if err != nil {
		t.Fatalf("runDaemonInstall: %v (stderr=%q)", err, errBuf.String())
	}

	// Unit file must not contain the token.
	unit, err := os.ReadFile(fake.path)
	if err != nil {
		t.Fatalf("unit file: %v", err)
	}
	if bytes.Contains(unit, []byte(tok)) {
		t.Errorf("unit file leaked the token:\n%s", unit)
	}

	// daemon.env must contain it, at mode 0600.
	envPath := filepath.Join(root, daemonEnvFileName)
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("daemon.env not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("daemon.env mode = %o, want 0600", perm)
	}
	got, err := readDaemonEnvToken(root)
	if err != nil {
		t.Fatalf("readDaemonEnvToken: %v", err)
	}
	if got != tok {
		t.Errorf("readDaemonEnvToken = %q, want %q", got, tok)
	}
	// The token must never have reached stderr.
	if strings.Contains(errBuf.String(), tok) {
		t.Errorf("token leaked to stderr: %q", errBuf.String())
	}
}

// TestDaemonReadsTokenFromEnvFile covers the runDaemon fallback: with a 0600
// <root>/daemon.env present and $GOFER_TOKEN unset, a non-loopback --listen
// passes ValidateListen (it fails later, at model resolution — proof it got
// past the token check), and no token string ever reaches stderr.
func TestDaemonReadsTokenFromEnvFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GOFER_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	root := t.TempDir()

	const tok = "envfile-token"
	if err := writeDaemonEnvToken(root, tok); err != nil {
		t.Fatalf("writeDaemonEnvToken: %v", err)
	}

	var out, errBuf bytes.Buffer
	got := run([]string{"daemon", "--listen", "192.168.1.50:7333", "--root", root}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run(daemon, env-file token) = %d, want 1 (fails later at model resolution)\nstderr: %s", got, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "refusing to bind") {
		t.Errorf("stderr = %q, want no refusing-to-bind error once daemon.env supplies the token", errBuf.String())
	}
	if strings.Contains(errBuf.String(), tok) || strings.Contains(out.String(), tok) {
		t.Errorf("token leaked to output: stdout=%q stderr=%q", out.String(), errBuf.String())
	}
}

// TestReadDaemonEnvTokenMissing asserts a missing daemon.env is a clean empty
// read, not an error.
func TestReadDaemonEnvTokenMissing(t *testing.T) {
	got, err := readDaemonEnvToken(t.TempDir())
	if err != nil {
		t.Fatalf("readDaemonEnvToken(missing) err = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("readDaemonEnvToken(missing) = %q, want empty", got)
	}
}

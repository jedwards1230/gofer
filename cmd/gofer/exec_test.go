package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
)

const execTestModel = "test-model"

// scriptedProvider is a deterministic, hermetic provider.Provider — mirrors
// agent-sdk-go/runner's own test fake: each call to Stream consumes the next
// scripted event sequence in order, so exec tests never touch the network.
type scriptedProvider struct {
	calls  int
	events [][]provider.StreamEvent
}

func (p *scriptedProvider) Stream(_ context.Context, _ provider.Request) (provider.StreamHandle, error) {
	if p.calls >= len(p.events) {
		return nil, fmt.Errorf("scriptedProvider: unexpected call %d (scripted for %d)", p.calls+1, len(p.events))
	}
	evs := p.events[p.calls]
	p.calls++
	return provider.SliceStream(evs...), nil
}

func (p *scriptedProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: execTestModel, Provider: "test"}
}

// execSeqClock and execSeqIDGen give exec tests deterministic, monotonic
// journal timestamps and ids without depending on wall-clock ordering.
func execSeqClock() func() time.Time {
	tm := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		tm = tm.Add(time.Second)
		return tm
	}
}

func execSeqIDGen() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%04d", n)
	}
}

// withScriptedExec installs execRunnerOpts for the test's duration, injecting
// a scripted provider plus deterministic id/clock seams — runExec's only hook
// for a test to avoid a real provider or network call. Reset on cleanup so it
// never leaks into another test.
func withScriptedExec(t *testing.T, prov *scriptedProvider) {
	t.Helper()
	idGen, clock := execSeqIDGen(), execSeqClock()
	execRunnerOpts = func(o *runner.Options) {
		o.Provider = prov
		o.IDGen = idGen
		o.Clock = clock
	}
	t.Cleanup(func() { execRunnerOpts = nil })
}

// oneTextTurn scripts a single turn: a short reasoning delta, then one text
// delta per element of final (their concatenation is the turn's settled
// text) — the deterministic shape every exec test below drives.
func oneTextTurn(final ...string) *scriptedProvider {
	deltas := []provider.StreamEvent{{Type: provider.StreamReasoningDelta, Text: "thinking"}}
	for _, s := range final {
		deltas = append(deltas, provider.StreamEvent{Type: provider.StreamTextDelta, Text: s})
	}
	deltas = append(deltas, provider.StreamEvent{
		Type: provider.StreamFinished, StopReason: provider.StopEndTurn,
		Usage: provider.Usage{InputTokens: 3, OutputTokens: 2},
	})
	return &scriptedProvider{events: [][]provider.StreamEvent{deltas}}
}

// jsonlLines splits JSONL output into lines, failing the test if any line
// fails to parse as JSON — locks the well-formed-output, never-panic contract.
func jsonlLines(t *testing.T, out []byte) [][]byte {
	t.Helper()
	out = bytes.TrimRight(out, "\n")
	if len(out) == 0 {
		return nil
	}
	lines := bytes.Split(out, []byte("\n"))
	for i, line := range lines {
		var v any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %s", i, err, line)
		}
	}
	return lines
}

// lineKinds decodes each line's "type" field, in order.
func lineKinds(t *testing.T, lines [][]byte) []string {
	t.Helper()
	kinds := make([]string, len(lines))
	for i, line := range lines {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("line %d: decode type: %v", i, err)
		}
		kinds[i] = env.Type
	}
	return kinds
}

// TestRunExec_DeterministicJSONL drives a scripted turn through `gofer exec`
// and asserts the exact JSONL event sequence on stdout: the user-prompt echo
// Runner.Prompt publishes, the loop's reasoning/text turn, then
// turn.finished — plus the final text message's settled content.
func TestRunExec_DeterministicJSONL(t *testing.T) {
	root := t.TempDir()
	withScriptedExec(t, oneTextTurn("hello ", "world"))

	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "-m", execTestModel, "--root", root, "-p", "hi"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run() = %d, want 0\nstderr: %s", got, errBuf.String())
	}

	lines := jsonlLines(t, out.Bytes())
	wantKinds := []string{
		"message.started", "message.finished", // user prompt echo
		"turn.started",
		"message.started", "message.delta", "message.finished", // reasoning
		"message.started", "message.delta", "message.delta", "message.finished", // text
		"turn.finished",
	}
	kinds := lineKinds(t, lines)
	if len(kinds) != len(wantKinds) {
		t.Fatalf("got %d JSONL lines %v, want %d %v", len(kinds), kinds, len(wantKinds), wantKinds)
	}
	for i, want := range wantKinds {
		if kinds[i] != want {
			t.Errorf("line %d type = %q, want %q", i, kinds[i], want)
		}
	}

	var finalMsg struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(lines[len(lines)-2], &finalMsg); err != nil {
		t.Fatalf("decode final message.finished: %v", err)
	}
	if finalMsg.Kind != "text" || finalMsg.Content != "hello world" {
		t.Errorf("final message = %+v, want kind=text content=%q", finalMsg, "hello world")
	}
}

// TestRunExec_OutputSchemaAccept covers the --output-schema happy path: a
// final result satisfying the schema exits 0.
func TestRunExec_OutputSchemaAccept(t *testing.T) {
	root := t.TempDir()
	withScriptedExec(t, oneTextTurn(`{"answer":"42","done":true}`))

	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	schemaDoc := `{"type":"object","properties":{"answer":{"type":"string"},"done":{"type":"boolean"}},"required":["answer","done"]}`
	if err := os.WriteFile(schemaPath, []byte(schemaDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "-m", execTestModel, "--root", root, "--output-schema", schemaPath, "-p", "go"}, strings.NewReader(""), &out, &errBuf)
	if got != 0 {
		t.Fatalf("run() = %d, want 0\nstderr: %s", got, errBuf.String())
	}
	// The stream is still emitted in full even with a schema attached.
	jsonlLines(t, out.Bytes())
}

// TestRunExec_OutputSchemaReject covers the mismatch path: a final result
// violating the schema exits non-zero, reports the violation on stderr, and
// never panics — the JSONL stream on stdout is still well-formed.
func TestRunExec_OutputSchemaReject(t *testing.T) {
	root := t.TempDir()
	withScriptedExec(t, oneTextTurn(`{"answer":42}`)) // wrong type: want string

	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	schemaDoc := `{"type":"object","properties":{"answer":{"type":"string"}}}`
	if err := os.WriteFile(schemaPath, []byte(schemaDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "-m", execTestModel, "--root", root, "--output-schema", schemaPath, "-p", "go"}, strings.NewReader(""), &out, &errBuf)
	if got == 0 {
		t.Fatalf("run() = 0, want non-zero for a schema violation\nstdout: %s", out.String())
	}
	if !strings.Contains(errBuf.String(), "output schema") {
		t.Errorf("stderr = %q, want it to mention the output schema violation", errBuf.String())
	}
	jsonlLines(t, out.Bytes())
}

// TestRunExec_PromptSources covers both ways to give exec its prompt: the -p
// flag, and (when -p is absent) all of stdin — both must drive the same run
// and publish the identical user-prompt text.
func TestRunExec_PromptSources(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"from -p flag", []string{"-p", "hi there"}, ""},
		{"from stdin", nil, "hi there\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			withScriptedExec(t, oneTextTurn("ok"))

			args := append([]string{"exec", "-m", execTestModel, "--root", root}, tc.args...)
			var out, errBuf bytes.Buffer
			got := run(args, strings.NewReader(tc.stdin), &out, &errBuf)
			if got != 0 {
				t.Fatalf("run() = %d, want 0\nstderr: %s", got, errBuf.String())
			}

			lines := jsonlLines(t, out.Bytes())
			if len(lines) < 2 {
				t.Fatalf("got %d JSONL lines, want at least the user-prompt pair", len(lines))
			}
			var userMsg struct {
				Kind    string `json:"kind"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(lines[1], &userMsg); err != nil {
				t.Fatalf("decode user message.finished: %v", err)
			}
			if userMsg.Kind != "user" || userMsg.Content != "hi there" {
				t.Errorf("user message = %+v, want kind=user content=%q", userMsg, "hi there")
			}
		})
	}
}

// TestRunExec_EmptyPromptIsUsageError locks the empty-prompt contract: no -p
// (or a whitespace-only one) and nothing but whitespace on stdin is a usage
// error (exit 2) — the run never starts, so stdout stays empty.
func TestRunExec_EmptyPromptIsUsageError(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"empty stdin, no -p", nil, ""},
		{"whitespace stdin, no -p", nil, "  \n\t\n"},
		{"whitespace -p, empty stdin", []string{"-p", "   "}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"exec"}, tc.args...)
			var out, errBuf bytes.Buffer
			got := run(args, strings.NewReader(tc.stdin), &out, &errBuf)
			if got != 2 {
				t.Fatalf("run() = %d, want 2\nstderr: %s", got, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "no prompt") {
				t.Errorf("stderr = %q, want it to mention the missing prompt", errBuf.String())
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty (run must not start)", out.String())
			}
		})
	}
}

// TestRunExec_MalformedSchemaIsCleanError locks the compile-error half of the
// --output-schema contract (distinct from a validation mismatch): a schema
// document that fails to compile is a clean command error (exit 1) surfaced
// BEFORE the prompt is driven — stdout stays empty, and nothing panics.
func TestRunExec_MalformedSchemaIsCleanError(t *testing.T) {
	cases := []struct {
		name      string
		schemaDoc string
	}{
		{"invalid JSON", `{`},
		{"unsupported type", `{"type":"quaternion"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			prov := oneTextTurn("never reached")
			withScriptedExec(t, prov)

			schemaPath := filepath.Join(t.TempDir(), "schema.json")
			if err := os.WriteFile(schemaPath, []byte(tc.schemaDoc), 0o600); err != nil {
				t.Fatal(err)
			}

			var out, errBuf bytes.Buffer
			got := run([]string{"exec", "-m", execTestModel, "--root", root, "--output-schema", schemaPath, "-p", "go"}, strings.NewReader(""), &out, &errBuf)
			if got != 1 {
				t.Fatalf("run() = %d, want 1\nstderr: %s", got, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "schema") {
				t.Errorf("stderr = %q, want it to mention the schema", errBuf.String())
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty (compile fails before the stream starts)", out.String())
			}
			if prov.calls != 0 {
				t.Errorf("provider was called %d times, want 0 (compile error precedes Prompt)", prov.calls)
			}
		})
	}
}

// TestRunExec_JSONFalseIsUsageError locks design decision #2: exec output is
// always JSONL, so an explicit --json=false is a usage error (exit 2), not a
// silent mode switch.
func TestRunExec_JSONFalseIsUsageError(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "--json=false", "-p", "hi"}, strings.NewReader(""), &out, &errBuf)
	if got != 2 {
		t.Fatalf("run() = %d, want 2\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "always JSONL") {
		t.Errorf("stderr = %q, want it to mention JSONL", errBuf.String())
	}
}

// TestRunExec_PositionalArgsIsUsageError locks design decision #3: exec takes
// no positional prompt arguments (only -p or stdin), so any positional is a
// usage error (exit 2).
func TestRunExec_PositionalArgsIsUsageError(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "-p", "hi", "extra"}, strings.NewReader(""), &out, &errBuf)
	if got != 2 {
		t.Fatalf("run() = %d, want 2\nstderr: %s", got, errBuf.String())
	}
}

// TestRunExec_UnknownAgentIsCleanError locks design decision #1: --agent
// names a manifest registry gofer doesn't have yet, so any non-empty --agent
// is a clean command error (exit 1), not a usage error.
func TestRunExec_UnknownAgentIsCleanError(t *testing.T) {
	var out, errBuf bytes.Buffer
	got := run([]string{"exec", "--agent", "bogus", "-p", "hi"}, strings.NewReader(""), &out, &errBuf)
	if got != 1 {
		t.Fatalf("run() = %d, want 1\nstderr: %s", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `unknown agent "bogus"`) {
		t.Errorf("stderr = %q, want it to mention the unknown agent", errBuf.String())
	}
}

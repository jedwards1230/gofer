package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// driveDaemonSession drives one turn through a running daemon as its own ACP
// client — dogfooding the same surface a phone or editor client uses, so
// run/resume get no privileged path once a daemon is present (CLAUDE.md
// invariant 2). It performs the handshake, opens the
// session (session/new for a fresh run, session/load for resumeID != ""),
// sends the prompt, and renders every streamed session/update until the
// turn's terminal PromptResponse. cmd is the invoking command's label
// ("run"/"resume") for the stderr progress prefix.
//
// c is caller-owned: the caller dialed it and is responsible for eventually
// closing it (see run.go/resume.go), since a dial failure needs to be
// distinguishable from a post-dial protocol failure. driveDaemonSession does
// close c itself once the prompt settles, though — see the comment at that
// call site — so the caller's own Close is a (harmless, idempotent) safety
// net for paths that never reach here.
func driveDaemonSession(ctx context.Context, c *daemon.Client, cmd, resumeID, cwd, prompt string, asJSON bool, stdout, stderr io.Writer) error {
	if _, err := c.Call(ctx, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion}); err != nil {
		return fmt.Errorf("daemon initialize: %w", err)
	}

	sessionID, err := openDaemonSession(ctx, c, resumeID, cwd)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "gofer %s: daemon session %s\n", cmd, shortID(sessionID))

	// Render notifications on their own goroutine, concurrently with the
	// blocking session/prompt call below. It runs until Notifications closes
	// — which happens when the connection closes, triggered explicitly below
	// once the prompt settles.
	renderDone := make(chan struct{})
	go func() {
		defer close(renderDone)
		if asJSON {
			renderDaemonUpdatesJSON(c, stdout)
			return
		}
		renderDaemonUpdatesHuman(c, stdout)
	}()

	// The prompt call runs against its own background context, NOT ctx: ctx
	// cancelling (Ctrl-C) must send session/cancel and then wait for the
	// daemon's real, terminal PromptResponse (StopReasonCancelled), exactly
	// as driveSession waits for the settled prefix rather than abandoning the
	// call locally the instant the signal fires.
	promptResult := make(chan callResult, 1)
	go func() {
		res, perr := c.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
		})
		promptResult <- callResult{res, perr}
	}()

	var result callResult
	select {
	case result = <-promptResult:
	case <-ctx.Done():
		_ = c.Notify(context.Background(), acp.MethodSessionCancel, acp.CancelNotification{SessionID: sessionID})
		result = <-promptResult
	}

	// Close now, before waiting on renderDone: this is what ends the
	// notification stream (Notifications closes when the connection does) and
	// lets the renderer goroutine return. One turn per daemon connection is
	// this M2 client's whole contract (see the package doc's "one outstanding
	// session/prompt per session" note) — there is no second prompt to lose by
	// closing here.
	_ = c.Close()
	<-renderDone

	if result.err != nil {
		return fmt.Errorf("session/prompt: %w", result.err)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(result.raw, &pr); err != nil {
		return fmt.Errorf("decode PromptResponse: %w", err)
	}
	if pr.StopReason == acp.StopReasonCancelled {
		_, _ = fmt.Fprintf(stderr, "gofer: interrupted — progress saved, resume with `gofer resume %s`\n", shortID(sessionID))
	}
	return nil
}

// callResult pairs a [daemon.Client.Call] result with its error so both can
// travel over one channel.
type callResult struct {
	raw json.RawMessage
	err error
}

// openDaemonSession creates a fresh session (resumeID == "") via session/new,
// or reopens an existing one via session/load, returning its id.
func openDaemonSession(ctx context.Context, c *daemon.Client, resumeID, cwd string) (string, error) {
	if resumeID == "" {
		result, err := c.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd})
		if err != nil {
			return "", fmt.Errorf("session/new: %w", err)
		}
		var sess acp.NewSessionResponse
		if err := json.Unmarshal(result, &sess); err != nil {
			return "", fmt.Errorf("decode session/new response: %w", err)
		}
		return sess.SessionID, nil
	}
	if _, err := c.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: resumeID, Cwd: cwd}); err != nil {
		return "", fmt.Errorf("session/load %s: %w", shortID(resumeID), err)
	}
	return resumeID, nil
}

// renderDaemonUpdatesHuman drains c's notifications through an [acpRenderer]
// until the channel closes (connection close). Non-session/update
// notifications (none exist in M2's daemon, which only ever pushes
// session/update — see internal/daemon's package doc — but a future protocol
// addition should not crash this loop) are skipped.
func renderDaemonUpdatesHuman(c *daemon.Client, stdout io.Writer) {
	rnd := newACPRenderer(stdout, colorEnabled(stdout))
	for n := range c.Notifications() {
		if n.Method != acp.MethodSessionUpdate {
			continue
		}
		_ = rnd.render(n.Params)
	}
}

// renderDaemonUpdatesJSON drains c's notifications as JSONL, one
// {"method":...,"params":...} line per notification — the daemon-path
// counterpart of --json, though the wire shape is ACP's session/update JSON
// rather than the SDK's event.Event JSON driveSession's --json emits (a
// documented difference; see cmd/gofer's daemon-path notes in run.go).
func renderDaemonUpdatesJSON(c *daemon.Client, stdout io.Writer) {
	enc := json.NewEncoder(stdout)
	for n := range c.Notifications() {
		_ = enc.Encode(struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}{n.Method, n.Params})
	}
}

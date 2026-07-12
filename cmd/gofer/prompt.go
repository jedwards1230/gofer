package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// resolvePromptCtx is resolvePrompt made cancellable: when the prompt comes
// from a blocking stdin read (no args), the read runs on its own goroutine and
// the select abandons it if ctx is cancelled (Ctrl-C), returning ctx.Err().
// The goroutine — still blocked on stdin — is left to die with the process; it
// never holds up the exit. This is what keeps `gofer run` / bare `gofer`
// interruptible at the prompt instead of wedging on a non-ctx-aware read.
func resolvePromptCtx(ctx context.Context, args []string, stdin io.Reader) (string, bool, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), true, nil
	}

	type result struct {
		text string
		ok   bool
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		t, ok, err := resolvePrompt(nil, stdin)
		ch <- result{t, ok, err}
	}()

	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case r := <-ch:
		return r.text, r.ok, r.err
	}
}

// resolvePrompt returns the prompt text for a run: args joined by spaces
// when given, else one line read from stdin. ok is false when neither yields
// non-empty text, in which case the caller has no prompt to drive.
func resolvePrompt(args []string, stdin io.Reader) (text string, ok bool, err error) {
	if len(args) > 0 {
		return strings.Join(args, " "), true, nil
	}

	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", false, fmt.Errorf("read prompt from stdin: %w", err)
		}
		return "", false, nil
	}
	line := strings.TrimSpace(scanner.Text())
	return line, line != "", nil
}

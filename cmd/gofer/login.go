package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jedwards1230/agent-sdk-go/auth"
	"github.com/jedwards1230/agent-sdk-go/runner"
)

// maxCodeAttempts is how many times `gofer login` re-prompts for the pasted
// authorization code before giving up — one fat-fingered paste must not kill
// the flow. The PKCE verifier stays valid and the authorization code is
// unconsumed until a successful exchange, so re-prompting (and re-calling
// Redeem) is safe.
const maxCodeAttempts = 3

// validProvider reports whether id is a provider gofer knows how to
// authenticate — the same set runner resolves a run model from. It queries
// runner.SupportedProviders() at call time rather than caching it at package
// init, so there is no init-time dependency on the runner package.
func validProvider(id string) bool {
	for _, p := range runner.SupportedProviders() {
		if p == id {
			return true
		}
	}
	return false
}

// providerBlurbs is a short, human description of each provider's login
// flow, printed by `gofer login` with no provider argument.
var providerBlurbs = map[string]string{
	"anthropic": "subscription OAuth (Claude Pro/Max) — paste the code shown after authorizing; or --api-key to store ANTHROPIC_API_KEY",
	"openai":    "subscription OAuth (ChatGPT) — completes via a local browser redirect; or --api-key to store OPENAI_API_KEY",
}

// printLoginProviders writes a short usage + provider listing to w — the
// "point the user at a login screen" response to `gofer login` with no
// provider argument.
func printLoginProviders(w io.Writer) {
	_, _ = fmt.Fprint(w, "Usage: gofer login <provider> [--api-key]\n\nProviders:\n")
	for _, p := range runner.SupportedProviders() {
		blurb := providerBlurbs[p]
		if blurb == "" {
			blurb = fmt.Sprintf("set %s, or run with --api-key", runner.EnvVar(p))
		}
		_, _ = fmt.Fprintf(w, "  %-11s %s\n", p, blurb)
	}
}

// usageError marks an error as a usage problem (bad or missing arguments),
// which main maps to exit code 2 instead of the generic command-error code 1.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// parseArgs parses fs while allowing flags and positionals to interleave in
// any order. Go's flag package stops at the first non-flag token, so a bare
// fs.Parse(args) would reject the documented `gofer login anthropic --api-key`
// form. This loops: parse, peel the leading positional, parse the remainder,
// repeat — collecting every positional regardless of position.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			// flag.ErrHelp (-h/--help) is passed through unwrapped so callers
			// can treat it as a successful help print (exit 0); every other
			// parse failure is a usage error (exit 2, per main's contract),
			// not a generic command error (exit 1).
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{msg: err.Error()}
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
}

// parsePositionals runs parseArgs and folds flag.ErrHelp into a nil error: the
// flag package already printed usage to the command's stderr, so -h/--help is a
// successful exit. Every command's flag parsing routes through here so exit
// codes stay consistent (0 help / 2 usage / 1 command error).
func parsePositionals(fs *flag.FlagSet, args []string) (positionals []string, help bool, err error) {
	positionals, err = parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil, true, nil
	}
	return positionals, false, err
}

// parseFlags parses fs and classifies the outcome for the exit-code contract:
// help is true when -h/--help was requested (the flag package already printed
// usage, so the command exits 0), and a parse failure is returned as a
// *usageError (exit 2) rather than a generic command error (exit 1). Commands
// with no interleaved positionals (run/resume/demo) use this; the auth commands
// use parsePositionals, which layers positional interleaving on the same rules.
func parseFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	switch e := fs.Parse(args); {
	case e == nil:
		return false, nil
	case errors.Is(e, flag.ErrHelp):
		return true, nil
	default:
		return false, &usageError{msg: e.Error()}
	}
}

// newAuthStore builds the auth.Store the login/logout/auth commands share.
// An empty root uses the store's default (~/.gofer).
func newAuthStore(root string) (*auth.Store, error) {
	var opts []auth.Option
	if root != "" {
		opts = append(opts, auth.WithRoot(root))
	}
	store, err := auth.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("open auth store: %w", err)
	}
	return store, nil
}

// runLogin implements `gofer login <provider> [--api-key] [--root dir]`.
func runLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apiKey := fs.Bool("api-key", false, "read an API key from stdin instead of running the OAuth login flow")
	root := fs.String("root", "", "override the auth store directory (default: ~/.gofer)")
	positionals, help, err := parsePositionals(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}

	if len(positionals) == 0 {
		// No provider named: point the user at a login screen rather than
		// erroring — this is the natural first stop for "how do I log in".
		printLoginProviders(stdout)
		return nil
	}
	if len(positionals) != 1 {
		return &usageError{msg: "usage: gofer login <anthropic|openai> [--api-key]"}
	}
	provider := positionals[0]
	if !validProvider(provider) {
		return &usageError{msg: fmt.Sprintf("unknown provider %q (want anthropic or openai)", provider)}
	}

	store, err := newAuthStore(*root)
	if err != nil {
		return err
	}
	if *apiKey {
		return loginWithAPIKey(store, provider, stdin, stdout)
	}
	return loginWithOAuth(ctx, store, provider, stdin, stdout, stderr)
}

// loginWithAPIKey reads a single line from stdin — never argv, which would
// leak the key to shell history and the process list — and stores it as a
// static API key.
func loginWithAPIKey(store *auth.Store, provider string, stdin io.Reader, stdout io.Writer) error {
	line, err := readLine(stdin)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return errors.New("empty api key")
	}
	if err := store.SetAPIKey(provider, key); err != nil {
		return fmt.Errorf("store api key: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "Stored API key for %s.\n", provider)
	return nil
}

// loginWithOAuth drives the subscription OAuth login. It never opens a
// browser: it prints the authorize URL and waits for the user to complete the
// flow, either by pasting back a code (manual mode) or by the SDK's local
// callback listener catching the redirect (callback mode).
func loginWithOAuth(ctx context.Context, store *auth.Store, provider string, stdin io.Reader, stdout, stderr io.Writer) error {
	login, err := store.Login(ctx, provider)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}
	defer login.Close()
	_, _ = fmt.Fprintf(stdout, "Open this URL in a browser to authorize:\n\n  %s\n\n", login.AuthorizeURL)

	switch login.Mode {
	case auth.LoginModeManualCode:
		if err := redeemWithRetry(login, stdin, stdout, stderr); err != nil {
			return err
		}
	case auth.LoginModeCallback:
		_, _ = fmt.Fprintln(stdout, "Waiting for the browser redirect to complete…")
		if err := login.Wait(); err != nil {
			return fmt.Errorf("wait for callback: %w", err)
		}
	default:
		return fmt.Errorf("unsupported login mode %v", login.Mode)
	}

	_, _ = fmt.Fprintf(stdout, "Logged in to %s.\n", provider)
	return nil
}

// redeemWithRetry prompts for the pasted authorization code and redeems it,
// re-prompting up to maxCodeAttempts times so a single bad paste (an empty
// line, the wrong text, or a code the endpoint rejects) doesn't kill the flow.
// It reads from ONE bufio.Reader for the whole loop so buffered input is never
// lost between attempts. An obviously-malformed paste (whitespace — e.g. a
// pasted shell command) is rejected locally, before any token-endpoint call.
func redeemWithRetry(login *auth.Login, stdin io.Reader, stdout, stderr io.Writer) error {
	reader := bufio.NewReader(stdin)
	var lastErr error

	for attempt := 1; attempt <= maxCodeAttempts; attempt++ {
		_, _ = fmt.Fprint(stdout, "Paste the code shown after authorizing: ")
		line, rerr := reader.ReadString('\n')
		eof := errors.Is(rerr, io.EOF)
		if rerr != nil && !eof {
			return fmt.Errorf("read pasted code: %w", rerr)
		}
		code := strings.TrimSpace(line)

		switch {
		case !looksLikeAuthCode(code):
			// (a) empty / (b) not a code#state or callback URL — reject before
			// contacting the endpoint (the PKCE verifier is untouched).
			lastErr = fmt.Errorf("that doesn't look like an authorization code")
			retryHint(stderr, attempt, eof, "that doesn't look like an authorization code")
		default:
			// (c) shape is plausible — try the endpoint. A rejection
			// (invalid_grant) is retryable: the verifier is still valid and the
			// real code is unconsumed.
			if err := login.Redeem(code); err == nil {
				return nil
			} else {
				lastErr = err
				retryHint(stderr, attempt, eof, "code rejected — paste the code exactly as shown")
			}
		}
		if eof {
			break // no more input coming
		}
	}
	return fmt.Errorf("login failed after %d attempt(s): %w", maxCodeAttempts, lastErr)
}

// retryHint prints the re-prompt line to stderr, unless this was the last
// attempt or input has ended (in which case the caller returns the error).
func retryHint(stderr io.Writer, attempt int, eof bool, reason string) {
	if attempt < maxCodeAttempts && !eof {
		_, _ = fmt.Fprintf(stderr, "%s (attempt %d/%d)\n", reason, attempt+1, maxCodeAttempts)
	}
}

// looksLikeAuthCode reports whether s is plausibly a pasted authorization code
// or callback URL: non-empty and free of internal whitespace. The common
// fat-finger is pasting a shell command (which contains spaces), so this
// catches it locally without a token-endpoint round trip; a real `code#state`
// or callback URL contains no whitespace.
func looksLikeAuthCode(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, unicode.IsSpace) < 0
}

// runLogout implements `gofer logout <provider> [--root dir]`.
func runLogout(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "override the auth store directory (default: ~/.gofer)")
	positionals, help, err := parsePositionals(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}

	if len(positionals) != 1 {
		return &usageError{msg: "usage: gofer logout <anthropic|openai>"}
	}
	provider := positionals[0]
	if !validProvider(provider) {
		return &usageError{msg: fmt.Sprintf("unknown provider %q (want anthropic or openai)", provider)}
	}

	store, err := newAuthStore(*root)
	if err != nil {
		return err
	}
	if err := store.Logout(provider); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "Logged out of %s.\n", provider)
	return nil
}

// runAuth implements `gofer auth [status] [--root dir]`. Bare `gofer auth`
// defaults to `status`.
func runAuth(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "override the auth store directory (default: ~/.gofer)")
	positionals, help, err := parsePositionals(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}

	// Bare `auth` defaults to `status`; the only accepted positional is `status`.
	if len(positionals) > 1 || (len(positionals) == 1 && positionals[0] != "status") {
		return &usageError{msg: "usage: gofer auth [status]"}
	}

	store, err := newAuthStore(*root)
	if err != nil {
		return err
	}
	entries, err := store.Status()
	if err != nil {
		return fmt.Errorf("read auth status: %w", err)
	}
	writeStatusTable(stdout, entries)
	return nil
}

// writeStatusTable renders an aligned "PROVIDER  KIND  STATUS" table sorted by
// provider name. It never prints token material — entries carry none.
func writeStatusTable(w io.Writer, entries []auth.StatusEntry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "No credentials configured.")
		return
	}

	sorted := make([]auth.StatusEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Provider < sorted[j].Provider })

	type row struct{ provider, kind, status string }
	rows := make([]row, 0, len(sorted)+1)
	rows = append(rows, row{"PROVIDER", "KIND", "STATUS"})
	for _, e := range sorted {
		rows = append(rows, row{e.Provider, string(e.Kind), statusText(e)})
	}

	var providerW, kindW int
	for _, r := range rows {
		if len(r.provider) > providerW {
			providerW = len(r.provider)
		}
		if len(r.kind) > kindW {
			kindW = len(r.kind)
		}
	}
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%-*s  %-*s  %s\n", providerW, r.provider, kindW, r.kind, r.status)
	}
}

// statusText renders a StatusEntry's STATUS column: "-" for API keys, and
// "valid until <RFC3339>" or "expired" for OAuth entries.
func statusText(e auth.StatusEntry) string {
	if e.Kind != auth.KindOAuth {
		return "-"
	}
	if e.Expired {
		return "expired"
	}
	return "valid until " + e.Expires.Format(time.RFC3339)
}

// readLine reads one line from r, stripping the trailing newline handling to
// the caller (via strings.TrimSpace). EOF without a trailing newline still
// returns whatever was read.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

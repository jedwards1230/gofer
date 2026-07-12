# M2 proof: an ACP client on a phone drives a session on the laptop

M2's bar (`docs/PRD.md`): "an ACP client on a phone drives a session on a
laptop." This is a live network test against a real tailnet, so you run it
personally — there is no hermetic CI equivalent for the phone-to-daemon leg
(the daemon's own WebSocket/JSON-RPC mechanics are covered by
`internal/daemon`'s in-process test suite: `go test -race ./internal/daemon/...`).

## 1. Start the daemon on the laptop

Bind it to the laptop's Tailscale address (not loopback — the whole point is
a phone reaching it over the tailnet), and set a bearer token so an
unauthenticated device on the tailnet can't drive your sessions:

```bash
# Mint a token once, store it somewhere you'll paste from (a password manager
# entry is fine — it never needs to be memorized).
openssl rand -hex 32

# Find your laptop's tailnet address:
tailscale ip -4

gofer daemon --listen <tailnet-ip>:7333 --token <the-token-you-minted>
```

Notes:

- `--token` also reads from `$GOFER_TOKEN`, so `GOFER_TOKEN=... gofer daemon
  --listen <tailnet-ip>:7333` works without the flag — handy for a launchd/
  systemd unit that keeps the token out of `ps` output.
- The token is **optional**: omitting it accepts any connection that can
  reach the listen address. On a tailnet with sane ACLs this is a reasonable
  default; a token adds a second factor beyond "on the tailnet."
- `gofer daemon` prints the listen address on startup — never the token.
- `--model` picks the model new ACP sessions use; omitted, it resolves the
  same way `gofer run` does (the sole logged-in provider's model — log in
  first with `gofer login <provider>` if none is configured, or pass
  `--model` explicitly if more than one is).
- `wss://` (TLS) is **not** terminated by gofer itself — it speaks plain
  `ws://`. Front it with a TLS terminator (e.g. a Tailscale Serve/Funnel
  target, or any reverse proxy) if the client requires `wss://`; on a private
  tailnet, plain `ws://` over the encrypted tailnet transport is already the
  common case and needs nothing extra.

## 2. Point the iOS ACP client at it

In the client's connection settings:

- **URL**: `ws://<tailnet-ip>:7333` (or `wss://<host>` if you fronted it with
  a TLS terminator per the note above).
- **Bearer token**: the token from step 1, if you set one. The daemon accepts
  it either as a standard `Authorization: Bearer <token>` header (preferred —
  use this if the client exposes a headers field) or, for a client that can
  only put a WebSocket URL together, a `?token=<token>` query parameter on
  the same URL.

## 3. Drive a session

From the client:

1. Connect — this performs the ACP `initialize` handshake; the daemon reports
   protocol version 1.
2. Create a new session (ACP `session/new`), pointed at a working directory
   the daemon process can read/write (a scratch checkout on the laptop, not a
   path that only exists on the phone).
3. Send a prompt (ACP `session/prompt`), e.g. "list the files in this
   directory and summarize what this project is."
4. Watch the response stream in: reasoning and text arrive as incremental
   `session/update` notifications as the model generates them; tool calls
   (e.g. a directory listing) appear as they start and again when they
   settle. The client's prompt request resolves once the turn reaches a
   terminal stop reason.
5. Send a follow-up prompt in the same session — the conversation continues
   with full prior context, exactly like `gofer resume` locally.
6. Interrupt a long-running turn from the client (ACP `session/cancel`) and
   confirm the in-flight response stops promptly rather than running to
   completion.

## 4. Confirm the laptop-side roster sees it live

The phone and the laptop drive the **same** `internal/supervisor.Supervisor`
instance inside the running `gofer daemon` process — the daemon's ACP layer and
every other client are peers of one supervisor ("everything is a client";
`CLAUDE.md` invariant 2). So while the phone-driven session is running, the
daemon's own roster already includes it, with the same status
(working/needs-input) and cost/usage a locally-started session would show.

The laptop-side clients that read that roster from the running daemon land
across the rest of the M2 stack:

- **`gofer ps` / `gofer kill` / `gofer archive`** (the CLI-over-daemon client,
  `internal/daemon.Client` + `cmd/gofer`) query and drive the daemon via its
  `gofer/roster`, `gofer/ps`, `gofer/kill`, and `gofer/archive` control methods
  on the same WebSocket listener — point them at the phone's daemon with
  `--daemon <tailnet-ip>:7333 --token <the-token>` and the phone-created
  session shows up in `gofer ps`, right alongside anything started locally.
  `gofer run`/`gofer resume <id> <prompt>` do the same auto-detection: with a
  daemon reachable at `--daemon` they drive the turn through it as their own
  ACP client instead of starting an in-process session. On that path a few
  in-process flags don't apply and say so on stderr — `-m` and `--root` are the
  daemon's (chosen at its startup), `--json` emits ACP `session/update` JSON
  rather than the SDK's `event.Event` JSONL, and the interactive attach TUI is
  replaced by plain streaming. Pass `--local` (alias `--no-daemon`) to force
  the in-process path even when a daemon is up.
- **The `gofer` TUI overview and `gofer attach`** (`internal/daemonbridge`,
  a `tui.Supervisor` backed by `internal/daemon.Client`) attach to the daemon
  and render its live roster + transcript, so the phone-created session
  appears and can be peeked/attached exactly like a local one. Bare `gofer`
  on an interactive terminal prefers a reachable daemon at the default
  address (honoring `$GOFER_TOKEN`) and falls back to the local in-process
  supervisor only when none is reachable, printing which backend it picked on
  stderr; `gofer attach [<session>]` is the explicit daemon-only form
  (`--daemon`/`--token` like `ps`/`kill`/`archive`) — no daemon reachable is a
  hard error there, never a silent local fallback. Point either at the
  phone's daemon (`--daemon <tailnet-ip>:7333 --token <the-token>` for
  `attach`) and the phone-created session shows up in the roster, right
  alongside anything started locally.

  **Fidelity**: the roster itself is exact (it's the daemon's own
  `gofer/roster` response, unmarshaled directly). The attached transcript is
  reconstructed client-side from the ACP `session/update` stream a
  `session/prompt` call yields — there is no direct channel to the daemon's
  in-process `event.Broker`, only the wire — so it is a best-effort
  projection, not a byte-identical replay: assistant text/reasoning stream
  incrementally and settle exactly as local; a tool call shows its start and
  final result (no incremental tool-output streaming, since ACP's
  `tool_call_update` has no delta variant distinct from a full update); turn
  cost/usage show on the roster row (refreshed every second) rather than in
  the transcript itself, since ACP's `session/prompt` response carries no
  usage/cost. Permission approvals are not relayed by the daemon at all in
  M2 (that lands with M3's approvals UX) — nothing in the reconstructed
  transcript represents them yet. Crucially, this is not a stall risk for the
  phone test: M2 runs tools with **no approval gate** (the loop's permission
  hook seam is unwired until M3, so tool calls execute unconditionally — the
  same permissive behavior as local `gofer run`). A phone-driven, tool-using
  turn therefore **auto-proceeds to completion** and never hangs waiting for
  an approval that could not be delivered over the daemon. `gofer attach
  <session>` resolves the id against the live roster and opens the TUI
  directly on that session's attach screen (← backs out to the overview);
  with no argument it opens overview-first.

You can also confirm the roster directly over the control channel: any
ACP/WebSocket client pointed at the same URL+token can call the `gofer/roster`
method and see the phone's session listed.

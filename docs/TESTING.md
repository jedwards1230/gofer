# gofer — testing strategy

The SDK owns loop/provider/session/permission testing (see agent-sdk-go's
`docs/TESTING.md`). gofer's layers:

| Layer | Type | CI | Approach |
|---|---|---|---|
| TUI | unit + golden | every push | two tiers: (1) `Update(msg)` → assert model state (the majority); (2) `x/exp/golden` vs `testdata/*.golden` for the few render-critical components, lipgloss direct — **no PTY**, fully deterministic. The `testkit` harness pins fixed sizes, forces `termenv.Ascii`, and uses `theme.Test()` |
| daemon · HTTP/SSE | integ | every push | real in-process server; drain real SSE into a channel; assert a liveness window (no-event-for-N-ms = still open). A route-coverage gate reflects registered vs tested routes |
| binary e2e | e2e | gated | build the real binary, spawn N clients against a temp socket (race regression). Skipped under `-short` and on Windows; runs on the full (push) lane |

## Rules

- Golden-file tests come **first** for any new render-critical component —
  before styling work, never after.
- Script turns in code (typed builders); JSONL fixtures only for captured
  session histories.
- Never test through a PTY; teatest is not a first move.

## CI

Fast PR lane (unit + golden); `go test -race` runs on push to main and
release tags (`.github/workflows/ci.yml`). The e2e socket test joins the
push lane when the daemon lands (M2).

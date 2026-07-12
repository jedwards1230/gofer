# M1 proof: kill and resume a real session

M1's bar (`docs/PRD.md`): "a real coding task, streaming, resumable after
kill." Two halves prove it — a manual run against a real provider (this repo
never touches the network, so a human runs this half), and an automated,
hermetic CI test that proves the same mechanics with a scripted provider.

## Manual proof (you run this)

1. **Authenticate.** Either:

   ```bash
   gofer login anthropic        # OAuth: paste the code it prints
   # or
   export ANTHROPIC_API_KEY=sk-...
   ```

2. **Start a real task**, in a scratch directory:

   ```bash
   cd /tmp/gofer-scratch
   gofer run -m claude-sonnet-5 "create hello.txt containing hi using your tools, then summarize"
   ```

   Watch the streamed transcript: reasoning, the `write` tool call, its
   result, then the summary. `gofer run` prints the journal path and session
   id to stderr before streaming starts — note the id.

3. **Kill it mid-run.** Ctrl-C (or `kill` the process) partway through. The
   turn in flight is interrupted, but every turn that had already settled —
   including a completed tool call and its result — is already fsynced to
   the journal; nothing settled is lost.

4. **Resume it:**

   ```bash
   gofer resume <id> "continue"
   ```

   The prior context (including the tool result) folds back into the
   provider's messages, and the conversation continues from where it left
   off.

5. **Inspect without resuming:**

   ```bash
   gofer resume <id>
   ```

   With no prompt, this prints the current transcript and exits — a
   read-only view. The journal itself is a plain, growing JSONL file at the
   path printed in step 2; `cat` it to see every settled entry.

## Automated CI proof

`internal/runner/runner_test.go`:

- **`TestRunner_KillAndResume`** — the milestone proof. A gofer-local
  scripted `provider.Provider` (`provider.SliceStream`, no network) drives a
  turn that calls the real builtin `read` tool against a temp file. The tool
  call's `Run` deterministically cancels the run's context right after the
  real tool executes (synchronously, in the loop's own goroutine — no timing
  race), simulating a kill the instant a tool round settles. The test then:
  - `Close()`s the runner (waiting for the journaling goroutine to drain) and
    reopens the journal from a **fresh** `session.FileStore` — bypassing any
    in-process cache — to prove the settled prefix (the user message and the
    tool round, with the tool's *real* file-content result) is durable on
    disk, not just held in memory.
  - `runner.Resume`s the same session id with a second scripted provider,
    asserts the resumed runner's folded context already carries the prior
    tool result (the fold → provider-message projection round-trips), drives
    a new prompt, and asserts the journal grew with the continuation —
    verified again from a fresh store after `Close`.
- **`TestRunner_TextTurn`** — a plain (no tool call) turn: the user prompt
  and settled assistant reply (text + reasoning) both land as journal
  entries, and `Fold` projects them back losslessly.

Run it: `go test -race ./internal/runner/...`.

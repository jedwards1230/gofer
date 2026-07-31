package mcpconn

// awaitready_cost_test.go covers a cost the repo's benchmark gate cannot see at
// all: WAITING (gofer#315, item 5, and its meta-finding).
//
// scripts/bench.sh gates on allocs/op and B/op, deliberately — wall-clock on a
// shared runner is too noisy to threshold. That choice makes an entire class of
// regression structurally invisible, because a settle wait, a backoff, or a
// poll interval allocates nothing. gofer#313's root cause was in that class,
// and so is this.
//
// The cost. Session create calls [Manager.AwaitReady] bounded by
// config.MCP.ReadyTimeout (default 2s). AwaitReady returns when every enabled
// server has completed one connection ATTEMPT. A server that fails fast
// completes its attempt immediately and settles the Manager for good, so the
// wait is paid once. A server whose DIAL HANGS never completes an attempt —
// and ConnectTimeout defaults to 30s, so it can hang far longer than the ready
// bound — which leaves the Manager unsettled and makes every subsequent
// AwaitReady wait out its full bound again. The cost is therefore per session
// create, not once per process, and it rises with server count because any one
// bad server is enough.
//
// TestManager_AwaitReady_BoundsOnCallerCtx already pins the CEILING (the wait
// does not run past the caller's bound). What follows pins the two things it
// does not: that the wait is actually consumed rather than skipped, and that it
// RECURS.
//
// On the assertion shape. CONTRIBUTING warns that "a timeout is a ceiling, not
// a floor", and that asserting a lower bound on something whose contract is an
// upper bound tests nothing. That warning does not apply here, and the
// difference matters: the property under test is not "the dependency is slow",
// it is "this call consumed its whole bound instead of returning early", which
// is a genuine floor. The final phase of the test is the control that keeps it
// honest — with the server settled, the same call must return promptly, so the
// harness demonstrably can observe both outcomes.

import (
	"context"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/mcp"

	"github.com/jedwards1230/gofer/internal/config"
)

// readyBound is the per-call ready wait these assertions use in place of the
// 2s production default. Long enough that scheduling jitter cannot account for
// a full wait, short enough to run three of them in the ordinary test lane.
const readyBound = 150 * time.Millisecond

// awaitElapsed runs one bounded AwaitReady and reports how long it took.
func awaitElapsed(m *Manager) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), readyBound)
	defer cancel()
	start := time.Now()
	m.AwaitReady(ctx)
	return time.Since(start)
}

// TestAwaitReadyBurnsItsBoundOnEveryCallWhileAServerHangs is the wall-clock
// assertion the allocation gate cannot make. It is a COVERAGE test: it
// describes what gofer does today, and exists so that a change to when the
// Manager settles has something to fail.
func TestAwaitReadyBurnsItsBoundOnEveryCallWhileAServerHangs(t *testing.T) {
	fc := newFakeCluster()
	fc.setUp("hangs", []fakeTool{echoTool("t")}, true)

	// A dial that hangs rather than failing. This is the case the fast-failing
	// fake in the tests next door does NOT reach: a failed attempt still counts
	// as an attempt and settles the Manager, so only a HANGING dial keeps it
	// unsettled.
	release := make(chan struct{})
	hangingDial := func(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fc.dial(ctx, srv, connectTimeout, callTimeout)
	}

	cfg := config.MCP{Servers: []config.MCPServer{{Name: "hangs", Command: "fake"}}}
	m := newTestManager(t, cfg, hangingDial)
	m.Start(context.Background())

	// Three creates in a row, as a user opening three sessions would.
	var total time.Duration
	for i := range 3 {
		elapsed := awaitElapsed(m)
		total += elapsed
		if elapsed < readyBound*9/10 {
			t.Fatalf("AwaitReady call %d returned after %s, well inside its %s bound — the Manager settled, so this test is no longer measuring the hanging-server case and its remaining assertions are vacuous", i+1, elapsed, readyBound)
		}
		if elapsed > readyBound+2*time.Second {
			t.Fatalf("AwaitReady call %d took %s — it is meant to be bounded by the caller's ctx (%s), not by the dial", i+1, elapsed, readyBound)
		}
	}
	// The finding, stated as an assertion: the wait is NOT amortized across
	// creates. If it ever becomes a once-per-process cost, this is what fails.
	if total < readyBound*5/2 {
		t.Fatalf("three AwaitReady calls cost %s in total, less than 2.5x the %s per-call bound — the wait is no longer paid per call", total, readyBound)
	}
	t.Logf("three session creates against one hanging MCP server cost %s of pure waiting (bound %s each); at the production ReadyTimeout default of %s that is %s",
		total, readyBound, config.DefaultMCPReadyTimeout, 3*config.DefaultMCPReadyTimeout)

	// The control. Once the dial completes, the Manager settles and the SAME
	// call returns promptly. Without this, every assertion above would also
	// pass against a harness that could only ever measure a full wait.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Snapshot().Tools) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(m.Snapshot().Tools) == 0 {
		t.Fatal("server never connected after the dial was released")
	}
	if elapsed := awaitElapsed(m); elapsed > readyBound/3 {
		t.Fatalf("AwaitReady took %s after the Manager settled, want a prompt return — the fast path this test's control depends on is not working, so the slow-path assertions above prove nothing", elapsed)
	}
}

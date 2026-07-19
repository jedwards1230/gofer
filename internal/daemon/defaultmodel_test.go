package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// newDaemonWithResolver stands up a real daemon over a test supervisor with the
// given Config and returns a dialled client. Mirrors model_test.go's setup.
func newDaemonWithResolver(t *testing.T, cfg daemon.Config) *wsClient {
	t.Helper()
	sup := newTestSupervisor(t, fauxProvider)
	d := daemon.New(sup, cfg)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return dial(t, context.Background(), "ws"+srv.URL[len("http"):], nil)
}

// rosterModel reports the model gofer/roster records for sid — what the daemon
// ACTUALLY created the session with, as opposed to what it claims elsewhere.
func rosterModel(t *testing.T, c *wsClient, sid string) string {
	t.Helper()
	resp := c.request("gofer/roster", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/roster error: %+v", resp.Error)
	}
	var roster []sessionInfoWire
	if err := json.Unmarshal(resp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	for _, s := range roster {
		if s.ID == sid {
			return s.Model
		}
	}
	t.Fatalf("session %s missing from gofer/roster: %+v", sid, roster)
	return ""
}

// helloDefaultModel reports the defaultModel this daemon advertises over
// gofer/hello.
func helloDefaultModel(t *testing.T, c *wsClient) string {
	t.Helper()
	resp := c.request("gofer/hello", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/hello error: %+v", resp.Error)
	}
	var hello daemon.HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatalf("unmarshal hello: %v", err)
	}
	return hello.DefaultModel
}

// TestSessionNewObservesResolverChange is the core regression test for issue
// #156: a daemon must not freeze its default model at startup. The resolver
// stands in for the config file `/model` writes — changing what it returns is
// exactly what a `session.model` write does — and the SECOND session/new must
// pick the new value up without the daemon restarting.
func TestSessionNewObservesResolverChange(t *testing.T) {
	const startupModel = "claude-sonnet-4-5"
	const rewrittenModel = "claude-haiku-4-5"

	var current atomic.Value
	current.Store(startupModel)
	c := newDaemonWithResolver(t, daemon.Config{
		DefaultModel: startupModel,
		ResolveDefaultModel: func(context.Context) (string, error) {
			return current.Load().(string), nil
		},
	})

	if got := rosterModel(t, c, newSession(t, c, t.TempDir())); got != startupModel {
		t.Fatalf("first session model = %q, want the startup default %q", got, startupModel)
	}

	// The operator runs /model, which writes config.json. Nothing restarts.
	current.Store(rewrittenModel)

	if got := rosterModel(t, c, newSession(t, c, t.TempDir())); got != rewrittenModel {
		t.Errorf("session created after the config change used %q, want %q — the daemon froze its startup value (issue #156)", got, rewrittenModel)
	}
}

// TestGoferHelloTracksTheLiveDefaultModel is the "do not merely move the lie"
// test: whatever the daemon would now create a session with must also be what it
// advertises. A fix that changed session/new but left hello reporting the
// startup value would leave every attached TUI header untruthful.
func TestGoferHelloTracksTheLiveDefaultModel(t *testing.T) {
	const startupModel = "claude-sonnet-4-5"
	const rewrittenModel = "claude-haiku-4-5"

	var current atomic.Value
	current.Store(startupModel)
	c := newDaemonWithResolver(t, daemon.Config{
		DefaultModel: startupModel,
		ResolveDefaultModel: func(context.Context) (string, error) {
			return current.Load().(string), nil
		},
	})

	if got := helloDefaultModel(t, c); got != startupModel {
		t.Fatalf("hello defaultModel = %q, want %q", got, startupModel)
	}

	current.Store(rewrittenModel)

	if got := helloDefaultModel(t, c); got != rewrittenModel {
		t.Errorf("hello defaultModel = %q, want %q — what the daemon advertises must match what it acts on", got, rewrittenModel)
	}
	// And the two must agree with each other, not merely each change.
	if acted := rosterModel(t, c, newSession(t, c, t.TempDir())); acted != helloDefaultModel(t, c) {
		t.Errorf("daemon created a session with %q but advertises %q", acted, helloDefaultModel(t, c))
	}
}

// TestSessionLoadUsesTheLiveDefaultModel covers the third read site: session/load
// carries no model of its own, so it resolves to the daemon default too and must
// use the same live value.
//
// The session is KILLED first, deliberately. supervisor.Resume short-circuits on
// a session that is still live and returns it untouched
// (internal/supervisor/supervisor.go), so ResumeOptions.Model only takes effect
// on a COLD load — one re-hydrated from the journal. Loading a still-live
// session here would assert nothing about this change and pass either way.
func TestSessionLoadUsesTheLiveDefaultModel(t *testing.T) {
	const startupModel = "claude-sonnet-4-5"
	const rewrittenModel = "claude-haiku-4-5"

	var current atomic.Value
	current.Store(startupModel)
	c := newDaemonWithResolver(t, daemon.Config{
		DefaultModel: startupModel,
		ResolveDefaultModel: func(context.Context) (string, error) {
			return current.Load().(string), nil
		},
	})

	sid := newSession(t, c, t.TempDir())
	if resp := c.request("gofer/kill", map[string]string{"sessionId": sid}); resp.Error != nil {
		t.Fatalf("gofer/kill error: %+v", resp.Error)
	}

	current.Store(rewrittenModel)

	resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: t.TempDir()})
	if resp.Error != nil {
		t.Fatalf("session/load error: %+v", resp.Error)
	}
	if got := rosterModel(t, c, sid); got != rewrittenModel {
		t.Errorf("cold-loaded session model = %q, want the live default %q", got, rewrittenModel)
	}
}

// TestDefaultModelResolverDegradesNonFatally pins the degradation contract the
// design calls for: a resolver that fails (a malformed config.json) or answers
// empty must NOT fail session/new. The daemon keeps serving on its startup
// value — an unrelated typo in a config file is not a reason to refuse to
// create sessions.
func TestDefaultModelResolverDegradesNonFatally(t *testing.T) {
	const startupModel = "claude-sonnet-4-5"

	tests := []struct {
		name     string
		resolver func(context.Context) (string, error)
	}{
		{
			name: "resolver error (malformed config)",
			resolver: func(context.Context) (string, error) {
				return "", errors.New("config: parse config.json: unexpected end of JSON input")
			},
		},
		{
			name: "resolver returns empty",
			resolver: func(context.Context) (string, error) {
				return "", nil
			},
		},
		{
			name: "resolver returns a value AND an error (error wins)",
			resolver: func(context.Context) (string, error) {
				return "should-be-ignored", errors.New("boom")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDaemonWithResolver(t, daemon.Config{
				DefaultModel:        startupModel,
				ResolveDefaultModel: tt.resolver,
			})

			// The request must SUCCEED — that is the load-bearing half.
			sid := newSession(t, c, t.TempDir())
			if got := rosterModel(t, c, sid); got != startupModel {
				t.Errorf("degraded session model = %q, want the startup default %q", got, startupModel)
			}
			if got := helloDefaultModel(t, c); got != startupModel {
				t.Errorf("degraded hello defaultModel = %q, want %q", got, startupModel)
			}
		})
	}
}

// TestNilResolverKeepsStartupModel asserts the pinned path — a daemon started
// with an explicit --model passes a nil resolver — behaves exactly as it always
// did, and never consults anything per request.
func TestNilResolverKeepsStartupModel(t *testing.T) {
	const pinnedModel = "claude-sonnet-4-5"
	c := newDaemonWithResolver(t, daemon.Config{DefaultModel: pinnedModel})

	if got := rosterModel(t, c, newSession(t, c, t.TempDir())); got != pinnedModel {
		t.Errorf("session model = %q, want the pinned %q", got, pinnedModel)
	}
	if got := helloDefaultModel(t, c); got != pinnedModel {
		t.Errorf("hello defaultModel = %q, want the pinned %q", got, pinnedModel)
	}
}

// TestClientSuppliedModelStillWins guards against the re-resolution swallowing
// an explicit per-session choice: a session/new naming its own model must be
// honored regardless of what the daemon's default currently resolves to.
func TestClientSuppliedModelStillWins(t *testing.T) {
	const requested = "claude-opus-4-1"
	c := newDaemonWithResolver(t, daemon.Config{
		DefaultModel: "claude-sonnet-4-5",
		ResolveDefaultModel: func(context.Context) (string, error) {
			return "claude-haiku-4-5", nil
		},
	})

	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: requested})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	if got := rosterModel(t, c, decodeSessionID(t, resp)); got != requested {
		t.Errorf("session model = %q, want the client-supplied %q", got, requested)
	}
}

// TestDefaultModelResolverConcurrentUse exercises the resolver from several
// peers at once, since Config.ResolveDefaultModel documents a concurrency
// requirement. Meaningful under -race, which the gate runs.
func TestDefaultModelResolverConcurrentUse(t *testing.T) {
	var calls atomic.Int64
	c := newDaemonWithResolver(t, daemon.Config{
		DefaultModel: "claude-sonnet-4-5",
		ResolveDefaultModel: func(context.Context) (string, error) {
			calls.Add(1)
			return "claude-haiku-4-5", nil
		},
	})

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			// hello takes no params and never mutates session state, so it is the
			// safe method to hammer concurrently over one connection.
			resp := c.request("gofer/hello", nil)
			if resp.Error != nil {
				t.Errorf("gofer/hello error: %+v", resp.Error)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got < n {
		t.Errorf("resolver called %d times for %d hellos, want at least %d (it must be consulted per request, not cached)", got, n, n)
	}
}

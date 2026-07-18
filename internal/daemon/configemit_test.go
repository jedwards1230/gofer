package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// configOptionUpdateParams is the wire decode of a config_option_update
// session/update: the discriminator plus the full config-options snapshot the
// peer receives. It decodes configOptions into []acp.ConfigOption — the SDK's
// own type, which owns the flattened "type"/currentValue decode — so the test
// asserts the projected shape via the same union modelConfigOption's response
// uses, not a hand-rolled parallel decode.
type configOptionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string             `json:"sessionUpdate"`
		ConfigOptions []acp.ConfigOption `json:"configOptions"`
	} `json:"update"`
}

// waitConfigOptionUpdate blocks for c's next config_option_update session/update
// notification, silently skipping every other frame (content chunks, gofer/
// event, the title update, ...), and returns its decoded params.
func waitConfigOptionUpdate(t *testing.T, c *wsClient) configOptionUpdateParams {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatalf("connection closed waiting for a config_option_update")
			}
			if f.Method != acp.MethodSessionUpdate {
				continue
			}
			var up configOptionUpdateParams
			if err := json.Unmarshal(f.Params, &up); err != nil {
				continue // not a config-shaped update (e.g. a content chunk)
			}
			if up.Update.SessionUpdate != "config_option_update" {
				continue
			}
			return up
		case <-deadline:
			t.Fatalf("timed out waiting for a config_option_update")
		}
	}
}

// assertNoConfigOptionUpdate asserts NO config_option_update arrives on c within
// a short grace window — the emit-on-change proof that a no-op model set fans
// nothing. Other frames (gofer/event, content, title) are expected and skipped.
func assertNoConfigOptionUpdate(t *testing.T, c *wsClient) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				return
			}
			if f.Method != acp.MethodSessionUpdate {
				continue
			}
			var up configOptionUpdateParams
			if err := json.Unmarshal(f.Params, &up); err != nil {
				continue
			}
			if up.Update.SessionUpdate == "config_option_update" {
				t.Errorf("unexpected config_option_update: %+v", up.Update.ConfigOptions)
				return
			}
		case <-deadline:
			return
		}
	}
}

// newSessionModel issues session/new over c with an explicit model and returns
// the new session id, failing the test on any error.
func newSessionModel(t *testing.T, c *wsClient, cwd, model string) string {
	t.Helper()
	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd, Model: model})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	return decodeSessionID(t, resp)
}

// assertModelConfigOption fails the test unless opts is exactly the single
// "model" select option with currentValue want and a non-empty catalog — the
// shared assertion both fan-out routes' tests reuse.
func assertModelConfigOption(t *testing.T, opts []acp.ConfigOption, want string) {
	t.Helper()
	if len(opts) != 1 {
		t.Fatalf("configOptions = %+v, want exactly one (model)", opts)
	}
	opt := opts[0]
	if opt.ID != "model" {
		t.Errorf("config option id = %q, want %q", opt.ID, "model")
	}
	if opt.Category != acp.ConfigCategoryModel {
		t.Errorf("config option category = %q, want %q", opt.Category, acp.ConfigCategoryModel)
	}
	sel, ok := opt.Kind.(acp.SelectKind)
	if !ok {
		t.Fatalf("config option kind = %T, want acp.SelectKind", opt.Kind)
	}
	if sel.CurrentValue != want {
		t.Errorf("select currentValue = %q, want %q", sel.CurrentValue, want)
	}
	if len(sel.Options) == 0 {
		t.Error("select options empty, want the provider-registry model catalog")
	}
}

// TestSessionSetConfigOptionFansConfigUpdate is the M5 live-config proof for the
// ACP route: when one attached peer changes the model via
// session/set_config_option, EVERY attached peer sees a config_option_update
// session/update carrying the model option with the NEW currentValue — for free,
// via acp.ToSessionUpdate's pass-through projection of the emitted
// event.ConfigOptionsUpdated. The driving peer already has the value in its
// set_config_option response; the observer (a phone watching what a laptop
// drives) gets it only through this fan-out.
func TestSessionSetConfigOptionFansConfigUpdate(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c1 := dial(t, context.Background(), url, nil)
	c2 := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSessionModel(t, c1, cwd, "claude-sonnet-5")

	// Both peers attach so both are in the session's fan-out set before the
	// change (session/load returns only after attachPeer has run).
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)

	setResp := c1.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: sid,
		ConfigID:  "model",
		Value:     acp.SelectValue{Value: "claude-opus-4-8"},
	})
	if setResp.Error != nil {
		t.Fatalf("session/set_config_option error: %+v", setResp.Error)
	}

	// The observer sees the change live as a config_option_update snapshot.
	obs := waitConfigOptionUpdate(t, c2)
	if obs.SessionID != sid {
		t.Errorf("config_option_update sessionId = %q, want %q", obs.SessionID, sid)
	}
	assertModelConfigOption(t, obs.Update.ConfigOptions, "claude-opus-4-8")

	// The driving peer is attached too, so it receives the same snapshot on the
	// event surface (in addition to its RPC response).
	drv := waitConfigOptionUpdate(t, c1)
	assertModelConfigOption(t, drv.Update.ConfigOptions, "claude-opus-4-8")
}

// TestGoferSetModelFansConfigUpdate is the same live-config proof for the
// gofer-native route: gofer/set_model advertises the change identically, so a
// gofer client (which drives model swaps via gofer/set_model, not the ACP
// set_config_option surface) still fans a config_option_update to every attached
// peer.
func TestGoferSetModelFansConfigUpdate(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c1 := dial(t, context.Background(), url, nil)
	c2 := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSessionModel(t, c1, cwd, "claude-sonnet-5")
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)

	setResp := c1.request("gofer/set_model", map[string]any{"sessionId": sid, "model": "claude-opus-4-8"})
	if setResp.Error != nil {
		t.Fatalf("gofer/set_model error: %+v", setResp.Error)
	}

	obs := waitConfigOptionUpdate(t, c2)
	if obs.SessionID != sid {
		t.Errorf("config_option_update sessionId = %q, want %q", obs.SessionID, sid)
	}
	assertModelConfigOption(t, obs.Update.ConfigOptions, "claude-opus-4-8")
}

// TestSetConfigOptionNoChangeFansNothing is the emit-on-change guard proof: a
// set_config_option that selects the model the session ALREADY has advertises no
// config_option_update — a no-op set must not storm attached clients with a
// snapshot that changed nothing. The response still succeeds (and still carries
// the option); only the fan-out is suppressed.
func TestSetConfigOptionNoChangeFansNothing(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c1 := dial(t, context.Background(), url, nil)
	c2 := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSessionModel(t, c1, cwd, "claude-opus-4-8")
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)

	// Re-select the current model: a genuine no-op.
	setResp := c1.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: sid,
		ConfigID:  "model",
		Value:     acp.SelectValue{Value: "claude-opus-4-8"},
	})
	if setResp.Error != nil {
		t.Fatalf("session/set_config_option error: %+v", setResp.Error)
	}

	assertNoConfigOptionUpdate(t, c2)
	assertNoConfigOptionUpdate(t, c1)
}

package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// TestSessionSetConfigOptionModel is the spec-surface proof for
// session/set_config_option: a configId="model" request maps to gofer's
// gofer-native model swap (the same effect as gofer/set_model), the roster
// reflects the new model, and the response advertises the "model" select
// option with its post-change current value and a non-empty option catalog.
func TestSessionSetConfigOptionModel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	sid := decodeSessionID(t, resp)

	setResp := c.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: sid,
		ConfigID:  "model",
		Value:     acp.SelectValue{Value: "claude-opus-4-8"},
	})
	if setResp.Error != nil {
		t.Fatalf("session/set_config_option error: %+v", setResp.Error)
	}

	var got acp.SetConfigOptionResponse
	if err := json.Unmarshal(setResp.Result, &got); err != nil {
		t.Fatalf("unmarshal SetConfigOptionResponse: %v", err)
	}
	if len(got.ConfigOptions) != 1 {
		t.Fatalf("configOptions = %+v, want exactly one (model)", got.ConfigOptions)
	}
	opt := got.ConfigOptions[0]
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
	if sel.CurrentValue != "claude-opus-4-8" {
		t.Errorf("select currentValue = %q, want claude-opus-4-8", sel.CurrentValue)
	}
	if len(sel.Options) == 0 {
		t.Error("select options empty, want the provider-registry model catalog")
	}
	var haveOpus bool
	for _, o := range sel.Options {
		if o.Value == "claude-opus-4-8" {
			haveOpus = true
		}
		if o.Value == "" || o.Name == "" {
			t.Errorf("select option missing value/name: %+v", o)
		}
	}
	if !haveOpus {
		t.Errorf("select options missing claude-opus-4-8: %+v", sel.Options)
	}

	// The set_model effect: the roster reflects the new model.
	rosterResp := c.request("gofer/roster", nil)
	if rosterResp.Error != nil {
		t.Fatalf("gofer/roster error: %+v", rosterResp.Error)
	}
	var roster []sessionInfoWire
	if err := json.Unmarshal(rosterResp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	var found bool
	for _, s := range roster {
		if s.ID != sid {
			continue
		}
		found = true
		if s.Model != "claude-opus-4-8" {
			t.Errorf("roster model after set_config_option = %q, want claude-opus-4-8", s.Model)
		}
	}
	if !found {
		t.Fatalf("session %s missing from gofer/roster: %+v", sid, roster)
	}
}

// TestSessionSetConfigOptionUnknownConfigID asserts an unrecognized configId is
// rejected as invalid params (a clear rpc error), never reaching the supervisor.
func TestSessionSetConfigOptionUnknownConfigID(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: sid,
		ConfigID:  "thermostat",
		Value:     acp.SelectValue{Value: "cozy"},
	})
	if resp.Error == nil {
		t.Fatal("session/set_config_option with unknown configId: want an error, got none")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

// TestSessionSetConfigOptionUnknownSession asserts a model change against a
// session id the supervisor has never seen surfaces as an application error —
// the same SetModel path gofer/set_model uses.
func TestSessionSetConfigOptionUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: "does-not-exist",
		ConfigID:  "model",
		Value:     acp.SelectValue{Value: "claude-opus-4-8"},
	})
	if resp.Error == nil {
		t.Fatal("session/set_config_option on unknown session: want an error, got none")
	}
}

// TestSessionSetConfigOptionRejectsBooleanValue asserts the "model" option
// requires a select value id: a boolean value is invalid params, never reaching
// the supervisor.
func TestSessionSetConfigOptionRejectsBooleanValue(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request(acp.MethodSessionSetConfigOption, acp.SetConfigOptionRequest{
		SessionID: sid,
		ConfigID:  "model",
		Value:     acp.BooleanValue{Value: true},
	})
	if resp.Error == nil {
		t.Fatal("session/set_config_option model with a boolean value: want an error, got none")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

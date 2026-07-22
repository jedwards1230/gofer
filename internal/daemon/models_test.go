package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// modelInfoWire mirrors the daemon's gofer/models wire shape (see
// internal/daemon/wire.go's modelInfoDTO), decoded for assertions.
type modelInfoWire struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	DisplayName   string `json:"displayName"`
	ContextWindow int    `json:"contextWindow"`
	MaxOutput     int    `json:"maxOutput"`
	Pricing       struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
	} `json:"pricing"`
	Reasoning bool `json:"reasoning"`
	Available bool `json:"available"`
	// Unregistered mirrors the DTO's omitempty flag: absent on the wire for a
	// registered model, present only for a synthesized record whose metadata is
	// unknown. Decoded here so the endpoint test can assert the key's ABSENCE
	// rather than being structurally blind to it.
	Unregistered bool `json:"unregistered,omitempty"`
}

// requestModels dials the daemon at url, calls gofer/models, and decodes the
// response into the wire slice.
func requestModels(t *testing.T, url string) []modelInfoWire {
	t.Helper()
	c := dial(t, context.Background(), url, nil)
	resp := c.request("gofer/models", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/models error: %+v", resp.Error)
	}
	var models []modelInfoWire
	if err := json.Unmarshal(resp.Result, &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	return models
}

// TestGoferModelsFieldsAvailabilityAndSort covers the full contract: every
// registered model is returned, each with populated metadata; availability is
// stamped per the host's authed providers; and the slice is sorted by
// (provider, id).
func TestGoferModelsFieldsAvailabilityAndSort(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemonWithConfig(t, sup, daemon.Config{
		DefaultModel: "faux",
		AuthedProviders: func() (map[string]bool, error) {
			return map[string]bool{"anthropic": true}, nil
		},
	})

	models := requestModels(t, url)

	// The returned id set equals provider.Models() — nothing missing or extra.
	want := map[string]bool{}
	for _, id := range provider.Models() {
		want[id] = true
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("gofer/models returned %d ids, want %d (%v)", len(got), len(want), provider.Models())
	}
	for id := range want {
		if !got[id] {
			t.Errorf("gofer/models missing registered model %q", id)
		}
	}

	// Every entry is well-formed and availability matches the authed set.
	for _, m := range models {
		if m.DisplayName == "" {
			t.Errorf("model %q: empty DisplayName", m.ID)
		}
		if m.Provider == "" {
			t.Errorf("model %q: empty Provider", m.ID)
		}
		if m.ContextWindow <= 0 {
			t.Errorf("model %q: ContextWindow = %d, want > 0", m.ID, m.ContextWindow)
		}
		switch m.Provider {
		case "anthropic":
			if !m.Available {
				t.Errorf("model %q (anthropic): Available = false, want true", m.ID)
			}
		default:
			if m.Available {
				t.Errorf("model %q (%s): Available = true, want false (only anthropic authed)", m.ID, m.Provider)
			}
		}
	}

	// The slice is sorted by (provider, id).
	for i := 1; i < len(models); i++ {
		prev, cur := models[i-1], models[i]
		if prev.Provider > cur.Provider || (prev.Provider == cur.Provider && prev.ID > cur.ID) {
			t.Fatalf("gofer/models not sorted at %d: %q/%q before %q/%q", i,
				prev.Provider, prev.ID, cur.Provider, cur.ID)
		}
	}

	// Spot-check a known model against the SDK registry rather than hardcoding.
	info, ok := provider.Lookup("claude-sonnet-5")
	if !ok {
		t.Fatal("provider.Lookup(claude-sonnet-5) missing — SDK registry changed")
	}
	var sonnet *modelInfoWire
	for i := range models {
		if models[i].ID == "claude-sonnet-5" {
			sonnet = &models[i]
			break
		}
	}
	if sonnet == nil {
		t.Fatal("gofer/models missing claude-sonnet-5")
		return
	}
	if sonnet.Provider != info.Provider {
		t.Errorf("claude-sonnet-5 Provider = %q, want %q", sonnet.Provider, info.Provider)
	}
	if sonnet.DisplayName != "Sonnet 5" {
		t.Errorf("claude-sonnet-5 DisplayName = %q, want %q", sonnet.DisplayName, "Sonnet 5")
	}
	if sonnet.ContextWindow != info.ContextWindow {
		t.Errorf("claude-sonnet-5 ContextWindow = %d, want %d", sonnet.ContextWindow, info.ContextWindow)
	}
	if sonnet.Pricing.Input != info.Pricing.Input || sonnet.Pricing.Output != info.Pricing.Output {
		t.Errorf("claude-sonnet-5 Pricing = %+v, want input %v output %v",
			sonnet.Pricing, info.Pricing.Input, info.Pricing.Output)
	}
	if !sonnet.Available {
		t.Error("claude-sonnet-5 Available = false, want true (anthropic authed)")
	}
}

// TestGoferModelsOmitsUnregisteredKey asserts at the ENDPOINT that no model
// gofer/models serves today carries an "unregistered" key: every registered
// model's metadata is real, so the omitempty safe case must fire all the way
// out to the wire. Decoding to a bool cannot tell absent from false, so this
// inspects the raw JSON keys.
//
// This is the standing regression twin of the one-time measurement that the
// flag is a zero-byte wire diff today: if a future SDK ships a synthesized
// record through provider.Models(), this fails loudly rather than letting a
// zero context window reach a client as though it were a real limit.
func TestGoferModelsOmitsUnregisteredKey(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemonWithConfig(t, sup, daemon.Config{
		DefaultModel: "faux",
		AuthedProviders: func() (map[string]bool, error) {
			return map[string]bool{"anthropic": true}, nil
		},
	})

	c := dial(t, context.Background(), url, nil)
	resp := c.request("gofer/models", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/models error: %+v", resp.Error)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("gofer/models returned no entries")
	}
	for _, obj := range raw {
		id := string(obj["id"])
		if _, present := obj["unregistered"]; present {
			t.Errorf("model %s carries an \"unregistered\" key; every model served today is registered", id)
		}
		// The metadata a client renders must be real, not a placeholder.
		for _, k := range []string{"contextWindow", "maxOutput"} {
			if _, present := obj[k]; !present {
				t.Errorf("model %s missing %s; registered models must carry real limits", id, k)
			}
		}
	}
}

// TestGoferModelsUnwiredAuthSeamAllUnavailable covers a daemon with no
// AuthedProviders closure: the full list still returns, every entry
// Available:false.
func TestGoferModelsUnwiredAuthSeamAllUnavailable(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	models := requestModels(t, url)

	if len(models) != len(provider.Models()) {
		t.Fatalf("gofer/models returned %d entries, want %d", len(models), len(provider.Models()))
	}
	for _, m := range models {
		if m.Available {
			t.Errorf("model %q: Available = true, want false (no auth seam wired)", m.ID)
		}
	}
}

// TestGoferModelsAuthErrorNonFatal covers an AuthedProviders closure that
// errors: gofer/models still returns the full list, all Available:false, and
// no rpc error.
func TestGoferModelsAuthErrorNonFatal(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemonWithConfig(t, sup, daemon.Config{
		DefaultModel: "faux",
		AuthedProviders: func() (map[string]bool, error) {
			return nil, errors.New("boom")
		},
	})

	models := requestModels(t, url)

	if len(models) != len(provider.Models()) {
		t.Fatalf("gofer/models returned %d entries, want %d", len(models), len(provider.Models()))
	}
	for _, m := range models {
		if m.Available {
			t.Errorf("model %q: Available = true, want false (auth closure errored)", m.ID)
		}
	}
}

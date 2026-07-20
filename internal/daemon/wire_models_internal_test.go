package daemon

import (
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// encodedKeys marshals v and returns its top-level object keys as a set, so a
// test can assert on the WIRE SHAPE (which keys exist) rather than on the Go
// struct field, which omitempty makes invisible.
func encodedKeys(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return obj
}

// TestModelInfoDTOUnregisteredOmitEmptyPolarity pins the wire polarity of the
// unregistered flag in ONE table: the key must be ABSENT for a registered
// (safe) model and PRESENT for an unregistered (dangerous) one. Inverting the
// flag — e.g. respelling it as a positive pricingKnown — would flip both rows
// and break a lenient client defaulting the missing key to false, which is
// exactly the failure this guard exists to catch. See modelInfoDTO.Unregistered.
func TestModelInfoDTOUnregisteredOmitEmptyPolarity(t *testing.T) {
	tests := []struct {
		name    string
		dto     modelInfoDTO
		wantKey bool // want the "unregistered" key present on the wire
		wantVal bool // its value when present
	}{
		{
			name: "registered model omits the key",
			dto: modelInfoDTO{
				ID: "claude-sonnet-5", Provider: "anthropic", DisplayName: "Sonnet 5",
				ContextWindow: 200000, MaxOutput: 64000,
				Pricing:   modelPricingDTO{Input: 3, Output: 15},
				Available: true,
			},
			wantKey: false,
		},
		{
			name: "unregistered model carries the key",
			dto: modelInfoDTO{
				ID: "claude-sonnet-99", Provider: "anthropic", DisplayName: "claude-sonnet-99",
				Available: true, Unregistered: true,
			},
			wantKey: true,
			wantVal: true,
		},
		{
			name: "unregistered wins over zero metadata alone",
			// A registered model could in principle carry zero metadata; the
			// flag, not the zero values, is what marks the record synthesized.
			dto: modelInfoDTO{
				ID: "zero-but-registered", Provider: "openai", DisplayName: "Zero",
			},
			wantKey: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := encodedKeys(t, tt.dto)
			raw, ok := obj["unregistered"]
			if ok != tt.wantKey {
				t.Fatalf("encoded %+v: unregistered key present = %v, want %v (keys: %v)",
					tt.dto, ok, tt.wantKey, keysOf(obj))
			}
			if !ok {
				return
			}
			var got bool
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal unregistered %s: %v", raw, err)
			}
			if got != tt.wantVal {
				t.Errorf("unregistered = %v, want %v", got, tt.wantVal)
			}
		})
	}
}

// TestToModelInfoDTOsRegisteredOmitUnregistered asserts the omitempty safe case
// actually fires against the LIVE registry: every model gofer/models serves
// today is registered, so none of them may carry the key. If a future SDK ships
// a synthesized record through provider.Models(), this fails loudly rather than
// letting a zero context window reach a client as if it were real.
func TestToModelInfoDTOsRegisteredOmitUnregistered(t *testing.T) {
	dtos := toModelInfoDTOs(map[string]bool{"anthropic": true})
	if len(dtos) == 0 {
		t.Fatal("toModelInfoDTOs returned no models — SDK registry empty?")
	}
	for _, dto := range dtos {
		t.Run(dto.ID, func(t *testing.T) {
			info, ok := provider.Lookup(dto.ID)
			if !ok {
				t.Fatalf("model %q not in the SDK registry", dto.ID)
			}
			if info.Unregistered {
				t.Fatalf("model %q is Unregistered in the SDK registry", dto.ID)
			}
			if dto.Unregistered {
				t.Errorf("dto.Unregistered = true, want false (registry says registered)")
			}
			if _, present := encodedKeys(t, dto)["unregistered"]; present {
				t.Errorf("registered model %q encoded an \"unregistered\" key; omitempty must drop it", dto.ID)
			}
		})
	}
}

// TestToModelInfoDTOsFromCopiesUnregistered drives the PRODUCTION projection
// with synthesized [provider.ModelInfo] records — including one carrying
// Unregistered, which the live registry never yields — and asserts the wire
// shape that comes out. Going through toModelInfoDTOsFrom rather than
// hand-building a modelInfoDTO is the whole point: this is what makes deleting
// the `Unregistered: info.Unregistered` line in the projection turn the suite
// red instead of leaving it green.
func TestToModelInfoDTOsFromCopiesUnregistered(t *testing.T) {
	infos := []provider.ModelInfo{
		{
			ID: "registered-shaped", Provider: "anthropic",
			ContextWindow: 200000, MaxOutput: 64000,
			Pricing: provider.Pricing{Input: 3, Output: 15},
		},
		{
			// Synthesized by provider.Resolve for an id newer than this
			// binary's registry: only ID and Provider are trustworthy.
			ID: "future-model-1", Provider: "openai", Unregistered: true,
		},
	}

	dtos := toModelInfoDTOsFrom(infos, map[string]bool{"anthropic": true})
	if len(dtos) != len(infos) {
		t.Fatalf("projected %d DTOs, want %d", len(dtos), len(infos))
	}

	byID := map[string]modelInfoDTO{}
	for _, d := range dtos {
		byID[d.ID] = d
	}

	t.Run("synthesized record carries the flag", func(t *testing.T) {
		dto, ok := byID["future-model-1"]
		if !ok {
			t.Fatal("future-model-1 missing from the projection")
		}
		if !dto.Unregistered {
			t.Error("dto.Unregistered = false; the projection dropped info.Unregistered")
		}
		obj := encodedKeys(t, dto)
		raw, ok := obj["unregistered"]
		if !ok {
			t.Fatalf("unregistered key missing on the wire (keys: %v)", keysOf(obj))
		}
		var got bool
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal unregistered %s: %v", raw, err)
		}
		if !got {
			t.Errorf("unregistered = %v on the wire, want true", got)
		}
		// Unknown, not zero: the numerics stay omitted so a client cannot
		// mistake a placeholder for a real limit.
		for _, k := range []string{"contextWindow", "maxOutput"} {
			if _, present := obj[k]; present {
				t.Errorf("%s present for an unregistered model; a zero limit must not reach the wire as a real one", k)
			}
		}
	})

	t.Run("registered record omits the flag", func(t *testing.T) {
		dto, ok := byID["registered-shaped"]
		if !ok {
			t.Fatal("registered-shaped missing from the projection")
		}
		if dto.Unregistered {
			t.Error("dto.Unregistered = true for a registered record")
		}
		obj := encodedKeys(t, dto)
		if _, present := obj["unregistered"]; present {
			t.Errorf("unregistered key present for a registered record; omitempty must drop it (keys: %v)", keysOf(obj))
		}
		// The real metadata survives the projection.
		if dto.ContextWindow != 200000 || dto.MaxOutput != 64000 {
			t.Errorf("metadata lost: ContextWindow = %d, MaxOutput = %d", dto.ContextWindow, dto.MaxOutput)
		}
		if dto.Pricing.Input != 3 || dto.Pricing.Output != 15 {
			t.Errorf("pricing lost: %+v", dto.Pricing)
		}
		if !dto.Available {
			t.Error("Available = false for an authed provider")
		}
	})
}

func keysOf(obj map[string]json.RawMessage) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	return out
}

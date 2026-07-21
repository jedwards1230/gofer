package daemon

// fleet_internal_test.go covers handleGoferFleet's optional-capability wiring
// directly (it lives in the internal test package so it can call the unexported
// handler and read the unexported DTO). A supervisor that implements
// [FleetUsager] yields {supported:true} with its total; one that does not yields
// {supported:false}, which is what lets an in-process daemon omit the fleet
// footer instead of reporting a misleading $0.

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// fleetSupNoAgg is a Supervisor that does NOT aggregate a fleet total — it
// embeds the interface (so it satisfies Supervisor) but adds no FleetUsage, so
// it does not satisfy FleetUsager. The embedded interface is nil, which is fine:
// handleGoferFleet only type-asserts, never calling a Supervisor method.
type fleetSupNoAgg struct{ Supervisor }

// fleetSupAgg is a Supervisor that DOES aggregate a fleet total.
type fleetSupAgg struct {
	Supervisor
	cost  provider.Cost
	usage provider.Usage
}

func (f fleetSupAgg) FleetUsage() (provider.Cost, provider.Usage) { return f.cost, f.usage }

func TestHandleGoferFleetSupported(t *testing.T) {
	d := New(fleetSupAgg{
		cost:  provider.Cost{USD: 0.5},
		usage: provider.Usage{InputTokens: 7, OutputTokens: 3},
	}, Config{ListenAddr: "127.0.0.1:0"})

	got, rerr := handleGoferFleet(d, context.Background(), nil, nil)
	if rerr != nil {
		t.Fatalf("handleGoferFleet: %v", rerr)
	}
	dto, ok := got.(fleetUsageDTO)
	if !ok {
		t.Fatalf("handleGoferFleet returned %T, want fleetUsageDTO", got)
	}
	if !dto.Supported {
		t.Error("fleetUsageDTO.Supported = false for a FleetUsager supervisor, want true")
	}
	if dto.Cost.USD != 0.5 {
		t.Errorf("fleetUsageDTO.Cost.USD = %v, want 0.5", dto.Cost.USD)
	}
	if dto.Usage.InputTokens != 7 || dto.Usage.OutputTokens != 3 {
		t.Errorf("fleetUsageDTO.Usage = %+v, want 7/3", dto.Usage)
	}
}

func TestHandleGoferFleetUnsupported(t *testing.T) {
	d := New(fleetSupNoAgg{}, Config{ListenAddr: "127.0.0.1:0"})

	got, rerr := handleGoferFleet(d, context.Background(), nil, nil)
	if rerr != nil {
		t.Fatalf("handleGoferFleet: %v", rerr)
	}
	dto, ok := got.(fleetUsageDTO)
	if !ok {
		t.Fatalf("handleGoferFleet returned %T, want fleetUsageDTO", got)
	}
	if dto.Supported {
		t.Error("fleetUsageDTO.Supported = true for a non-aggregating supervisor, want false")
	}
}

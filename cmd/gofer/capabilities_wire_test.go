package main

// capabilities_wire_test.go covers [classifyCapabilities] — the one rule that
// decides whether a /mcp or /skills panel shows a report or says UNKNOWN
// (gofer#303).
//
// It is tested at this seam for the same reason [classifyHelloDefault] is: the
// branch that matters is the unsupported one, and reaching it through a real
// client would mean standing up a daemon that deliberately omits the method.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/daemon"
)

// populatedSnapshot is a non-empty report, so a test can tell "the snapshot
// was returned" from "the zero value was returned" — which is the entire
// distinction the unknown branches turn on.
func populatedSnapshot() capability.Snapshot {
	return capability.Snapshot{MCP: capability.MCP{
		Servers:        []capability.Server{{Name: "github", ConfiguredTransport: "stdio", Enabled: true, Connected: true}},
		ConnectedTools: 3,
		SchemaMode:     "preload",
	}}
}

func TestClassifyCapabilitiesOK(t *testing.T) {
	want := populatedSnapshot()
	got, err := classifyCapabilities(want, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Known {
		t.Error("a successful read must be Known")
	}
	if len(got.Snapshot.MCP.Servers) != 1 || got.Snapshot.MCP.ConnectedTools != 3 {
		t.Errorf("snapshot must pass through verbatim, got %+v", got.Snapshot)
	}
}

// TestClassifyCapabilitiesUnsupportedIsUnknownNotAnError covers both ways a
// daemon says "I have no answer": the wrapped method-not-found a pre-
// gofer/capabilities daemon produces, and the bare sentinel a `--workers`
// router's {supported:false} produces. Neither is an error the user can act
// on, so both must arrive as a plain unknown.
func TestClassifyCapabilitiesUnsupportedIsUnknownNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"bare sentinel (--workers router)", daemon.ErrCapabilitiesUnsupported},
		{"wrapped method-not-found (old daemon)", fmt.Errorf("%w: -32601", daemon.ErrCapabilitiesUnsupported)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A populated snapshot is passed in deliberately: the classifier
			// must DISCARD it, not pair it with an unknown flag nobody reads.
			got, err := classifyCapabilities(populatedSnapshot(), tc.err)
			if err != nil {
				t.Errorf("unsupported must not surface as an error, got %v", err)
			}
			if got.Known {
				t.Error("unsupported must classify as UNKNOWN")
			}
			if len(got.Snapshot.MCP.Servers) != 0 {
				t.Errorf("an unknown answer must carry no snapshot, got %+v", got.Snapshot)
			}
		})
	}
}

// TestClassifyCapabilitiesErrorIsUnknownAndKeepsTheError pins the third
// branch: a transport failure is still UNKNOWN for rendering purposes, but the
// error is returned so exactly one place decides to swallow it.
func TestClassifyCapabilitiesErrorIsUnknownAndKeepsTheError(t *testing.T) {
	boom := errors.New("connection reset")
	got, err := classifyCapabilities(populatedSnapshot(), boom)
	if !errors.Is(err, boom) {
		t.Errorf("want the underlying error back, got %v", err)
	}
	if got.Known {
		t.Error("a failed read must classify as UNKNOWN")
	}
	if len(got.Snapshot.MCP.Servers) != 0 {
		t.Errorf("a failed read must carry no snapshot, got %+v", got.Snapshot)
	}
}

// TestClassifyCapabilitiesKnownEmptyIsNotUnknown is the inverse guard: a
// backend that successfully reported "nothing configured" must stay Known.
// Collapsing it to unknown would be honest-but-useless; collapsing unknown to
// it would be the lie. Both directions need pinning.
func TestClassifyCapabilitiesKnownEmptyIsNotUnknown(t *testing.T) {
	got, err := classifyCapabilities(capability.Snapshot{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Known {
		t.Error("an empty-but-successful report must be Known, not unknown")
	}
}

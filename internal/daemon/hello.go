package daemon

// WireVersion is the version of the router↔worker wire contract (design §6):
// the daemon's existing public wire (ACP v1 + gofer/*) plus gofer/hello. It
// starts at 1 and bumps only on a BREAKING change to that surface; additive
// fields/methods do not bump it (the wire tolerates unknown methods and
// additive fields — see internal/daemonbridge). Advertised in both the worker
// endpoint file and the gofer/hello handshake so a router can route around
// version skew.
const WireVersion = 1

// HelloResult is what gofer/hello returns: the responder's identity across the
// three version axes a router must reconcile (design §6). BinaryVersion is the
// daemon's build version (Config.Version), WireVersion the router↔worker wire
// contract version ([WireVersion]), and ACPProtocolVersion the ACP version this
// daemon speaks (acp.ProtocolVersion). A router calls gofer/hello first on
// every worker connection and uses these to make its adopt/skew-route decision.
type HelloResult struct {
	BinaryVersion      string `json:"binaryVersion"`
	WireVersion        int    `json:"wireVersion"`
	ACPProtocolVersion int    `json:"acpProtocolVersion"`

	// DefaultModel is the model this daemon creates a session with when the
	// client's session/new carries none ([Config.DefaultModel]). A client
	// showing "the model sessions will use" must read it from here rather than
	// resolve one locally: the daemon resolved its own at ITS startup against
	// ITS store, which a client machine cannot reproduce. Omitted (and decoded
	// as "") by daemons predating the field, which callers must treat as
	// "unknown", not "none".
	DefaultModel string `json:"defaultModel,omitempty"`
}

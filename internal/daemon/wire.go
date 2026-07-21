package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/modelmeta"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// configIDModel is the ACP session/set_config_option configId gofer maps to its
// gofer-native model swap (see handleSessionSetConfigOption). Model-setting
// stays gofer-native — gofer/set_model over [supervisor.Supervisor.SetModel] —
// per the PRD; this is only the stable ACP spec-surface entry point to it.
const configIDModel = "model"

// modelSelectOptions projects the SDK provider registry into the ACP select
// options for the "model" config option: the same catalog gofer/models exposes
// (see toModelInfoDTOs), sorted (provider, id), each labeled with its gofer
// display name. A model whose provider the daemon host has no credential for
// carries a description noting it is unavailable, so a client can still list it
// (grayed out) rather than silently dropping it.
func modelSelectOptions(authed map[string]bool) []acp.SelectOption {
	infos := toModelInfoDTOs(authed)
	opts := make([]acp.SelectOption, 0, len(infos))
	for _, m := range infos {
		opt := acp.SelectOption{Value: m.ID, Name: m.DisplayName}
		if !m.Available {
			opt.Description = "no " + m.Provider + " credential on this host"
		}
		opts = append(opts, opt)
	}
	return opts
}

// modelConfigOption builds the ACP "model" [acp.ConfigOption]: a select over the
// provider registry (see modelSelectOptions) whose CurrentValue is the session's
// current model. It is the single option gofer's session/set_config_option
// response advertises today (see handleSessionSetConfigOption).
func modelConfigOption(currentModel string, authed map[string]bool) acp.ConfigOption {
	return acp.ConfigOption{
		ID:       configIDModel,
		Name:     "Model",
		Category: acp.ConfigCategoryModel,
		Kind: acp.SelectKind{
			CurrentValue: currentModel,
			Options:      modelSelectOptions(authed),
		},
	}
}

// modelConfigOptionEvent builds the neutral, transport-only [event.ConfigOption]
// snapshot of the "model" selector — the event-contract twin of
// modelConfigOption (which builds the ACP shape a session/set_config_option
// response carries). It maps the SAME modelSelectOptions catalog into
// [event.ConfigSelectValue] and marks the session's current model as
// SelectedValue, so an emitted [event.ConfigOptionsUpdated] carrying it projects
// (via acp.ToSessionUpdate) to exactly the config_option_update the response
// advertises — a pass-through projection with no gofer-side ACP synthesis.
func modelConfigOptionEvent(currentModel string, authed map[string]bool) event.ConfigOption {
	sel := modelSelectOptions(authed)
	values := make([]event.ConfigSelectValue, 0, len(sel))
	for _, o := range sel {
		values = append(values, event.ConfigSelectValue{
			Value:       o.Value,
			Name:        o.Name,
			Description: o.Description,
		})
	}
	return event.ConfigOption{
		ID:            configIDModel,
		Name:          "Model",
		Category:      string(acp.ConfigCategoryModel),
		Kind:          event.ConfigOptionSelect,
		SelectedValue: currentModel,
		Values:        values,
	}
}

// NewSessionMeta is gofer's `_meta` extension on session/new, in BOTH
// directions. ACP reserves `_meta` for exactly this — implementation-specific
// data an unaware peer ignores — so gofer's model and subagent fields ride there
// rather than in new top-level fields the SDK's shared wire types would have to
// grow (contract-only consumption).
//
// It is EXPORTED, and every producer and consumer in this repo uses this one
// type: the daemon's session/new handler, internal/daemonbridge, the M6 router's
// router→worker call, and cmd/gofer's `gofer run`. The keys can only live in
// struct tags (Go tags cannot reference a constant), so a second declaration of
// this shape anywhere would be a second, independently-typo-able copy of the
// wire contract — with a mistake in the REQUEST direction failing silently, as a
// plain root session and no error. One type is the only way to close that.
//
// Request direction: ParentID and Agent state the client's INTENT (Model and
// Depth are ignored). Response direction: every field states what the daemon
// actually ASSIGNED — Depth in particular is derived daemon-side (parent + 1)
// and is not knowable by the client at all.
type NewSessionMeta struct {
	// Model is the model the daemon ASSIGNED to the new session: the client's
	// requested model when it sent one, else the daemon's own resolved default.
	// Empty only when the daemon could not resolve one at all. A client that sees
	// no `_meta` at all is talking to a daemon predating this field and falls
	// back to whatever it requested (see [NewSessionMeta.ModelOr]).
	Model string `json:"gofer/model,omitempty"`
	// ParentID names the session SPAWNING this one, making the new session a
	// subagent of it (see [supervisor.CreateOptions.ParentID]). An unknown parent
	// is rejected as invalid params, and one already at the depth cap as an
	// application error — a client learns which, rather than silently getting a
	// root session.
	ParentID string `json:"gofer/parent,omitempty"`
	// Agent is the new session's agent identity, stamped onto its tool-call
	// events (see [supervisor.CreateOptions.Agent]).
	Agent string `json:"gofer/agent,omitempty"`
	// Depth is the assigned depth in the subagent tree (response direction only).
	Depth int `json:"gofer/depth,omitempty"`
}

// ModelOr is the model the new session actually runs: the daemon's own answer,
// falling back to requested — what the client asked for — when the daemon sent
// no `_meta` at all.
//
// The fallback is ONLY for a daemon predating the field. It must never be read
// as "the request is as good as the response": on the normal path requested is
// "" (the client sends no model and lets the daemon decide), which is exactly
// why echoing it left the roster row modelless (issue #162, defect 2).
func (m *NewSessionMeta) ModelOr(requested string) string {
	if m != nil && m.Model != "" {
		return m.Model
	}
	return requested
}

// SubagentLink reports the assigned parent/agent/depth, nil-safe so a consumer
// needs no nil dance for the overwhelmingly common no-`_meta` response.
func (m *NewSessionMeta) SubagentLink() (parentID, agent string, depth int) {
	if m == nil {
		return "", "", 0
	}
	return m.ParentID, m.Agent, m.Depth
}

// NewSessionRequest is a session/new request: ACP's own
// [acp.NewSessionRequest] plus gofer's `_meta` extension. The embedded ACP
// request decodes exactly as it always did, so an ordinary client that sends no
// `_meta` produces a nil Meta and a plain root session.
type NewSessionRequest struct {
	acp.NewSessionRequest
	Meta *NewSessionMeta `json:"_meta,omitempty"`
}

// NewSessionRequestFor builds the session/new request every gofer client sends.
// It attaches `_meta` ONLY when a subagent link was actually asked for, so a
// plain create is byte-for-byte the request gofer sent before subagents existed
// — the property that keeps this additive for any peer on either end.
func NewSessionRequestFor(cwd, model, parentID, agent string) NewSessionRequest {
	req := NewSessionRequest{
		NewSessionRequest: acp.NewSessionRequest{Cwd: cwd, Model: model},
	}
	if parentID != "" || agent != "" {
		req.Meta = &NewSessionMeta{ParentID: parentID, Agent: agent}
	}
	return req
}

// NewSessionResponse is a session/new response: ACP's own
// [acp.NewSessionResponse] plus the `_meta` extension carrying what the daemon
// assigned.
//
// It exists because the ACP response carries only the session id. A client that
// let the daemon choose the model — the NORMAL path — therefore had no way to
// learn what it actually got, so internal/daemonbridge's Create could only echo
// back the model it had REQUESTED (the empty string), and the roster row it
// returned could never carry the real one (issue #162, defect 2). The subagent
// link is carried for the same reason.
type NewSessionResponse struct {
	acp.NewSessionResponse
	Meta *NewSessionMeta `json:"_meta,omitempty"`
}

// sessionInfoDTO is the wire shape for the gofer-native roster/ps methods. It
// is a deliberate subset of [supervisor.SessionInfo]: Summary, Pending, and
// Artifacts are reserved-and-always-zero in M2 (see the SessionInfo doc) and
// JournalPath is an on-disk implementation detail, so none of the three cross
// the wire.
type sessionInfoDTO struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	// Effort is the session's reasoning effort (see
	// [supervisor.SessionInfo.Effort]). Omitted when empty, which is both the
	// common case (no explicit level — the provider's default) and what an
	// older daemon sends; a client decoding this simply reads "" and shows the
	// level as unset.
	Effort string         `json:"effort,omitempty"`
	Cost   provider.Cost  `json:"cost"`
	Usage  provider.Usage `json:"usage"`
	Queued int            `json:"queued"`
	// Pending is the live count of outstanding permission requests for the
	// session (see [supervisor.SessionInfo.Pending]). Omitted when zero — an
	// older client that never reads it is unaffected, and a client that does
	// renders the ✋N approval glyph from it.
	Pending int       `json:"pending,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Project string    `json:"project,omitempty"`
	// Live is false for a disk-only (archived/offline) entry from gofer/ps;
	// always true from gofer/roster, which only ever returns live sessions.
	Live bool `json:"live"`
	// Cwd is the session's working directory (see [supervisor.SessionInfo.Cwd]),
	// added so a client can drive session/load's required cwd (see
	// handleSessionLoad) for a session it discovers via gofer/roster or
	// gofer/ps, rather than guessing — see internal/daemonbridge's
	// loadHistory. Additive field: an older client decoding this DTO simply
	// never reads it.
	Cwd string `json:"cwd,omitempty"`
	// BinaryVersion is the gofer build version of the process running the
	// session (see [supervisor.SessionInfo.BinaryVersion]) — under M6 worker
	// mode, the session's WORKER.
	//
	// It reaches the WIRE only: as of slice 3a no gofer client RENDERS it —
	// neither `gofer ps` nor the TUI roster/session-info shows it — so a mixed
	// binaryVersion roster is observable by decoding this field (or with a raw
	// gofer/ps call), not by looking at an operator's screen. Rendering it in
	// `gofer ps` and the TUI roster is deferred to slice 3b; until then §11's
	// "session/list shows mixed binaryVersions" demo criterion is satisfied at
	// raw-wire level only.
	//
	// Omitted when empty, which is the normal case for the in-process daemon and
	// for every disk-only (offline) row: it is live-only state, never journaled.
	BinaryVersion string `json:"binaryVersion,omitempty"`
	// ParentID, Agent and Depth are the session's subagent link (see
	// [supervisor.SessionInfo.ParentID]): which session spawned it, which agent
	// identity it runs as, and how deep it sits in the resulting tree. All three
	// are omitempty and all three are zero for a root session, so a roster of
	// ordinary sessions is byte-for-byte what it was before subagents existed and
	// an older client simply never reads them.
	ParentID string `json:"parentId,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}

func toSessionInfoDTO(info supervisor.SessionInfo) sessionInfoDTO {
	return sessionInfoDTO{
		ID:            info.ID,
		Title:         info.Title,
		Status:        info.Status.String(),
		Model:         info.Model,
		Effort:        info.Effort,
		Cost:          info.Cost,
		Usage:         info.Usage,
		Queued:        info.Queued,
		Pending:       info.Pending,
		Created:       info.Created,
		Updated:       info.Updated,
		Project:       info.Project,
		Live:          info.Live,
		Cwd:           info.Cwd,
		BinaryVersion: info.BinaryVersion,
		ParentID:      info.ParentID,
		Agent:         info.Agent,
		Depth:         info.Depth,
	}
}

func toSessionInfoDTOs(infos []supervisor.SessionInfo) []sessionInfoDTO {
	out := make([]sessionInfoDTO, len(infos))
	for i, info := range infos {
		out[i] = toSessionInfoDTO(info)
	}
	return out
}

// fleetUsageDTO is the wire shape for gofer/fleet: the fleet-wide Cost/Usage
// total across every live session (see [handleGoferFleet]).
//
// Supported carries the answer to "does this daemon aggregate a fleet total at
// all" as data rather than as a JSON-RPC error, so a client can distinguish an
// unsupported daemon (in-process, or one predating this method) from a supported
// daemon that genuinely has a $0 fleet — a semantic distinction an error code
// would collapse. An in-process daemon replies {supported:false}; a worker-mode
// daemon replies {supported:true, ...} even when the fleet is idle at $0.
// provider.Cost and provider.Usage already carry their own json tags, so they
// project verbatim (both are structs, so an unsupported reply still renders their
// zero values — the client reads Supported, not the zeroed totals).
type fleetUsageDTO struct {
	Supported bool           `json:"supported"`
	Cost      provider.Cost  `json:"cost"`
	Usage     provider.Usage `json:"usage"`
}

// modelInfoDTO is the wire shape for the gofer-native gofer/models method: a
// projection of [provider.ModelInfo] (the SDK provider registry's per-model
// metadata) plus the gofer-side DisplayName (see [modelmeta.DisplayName], which
// the SDK doesn't carry) and the daemon-computed Available flag. Its json tags
// are camelCase, mirroring [sessionInfoDTO]; [provider.Pricing] is projected
// through the tagged [modelPricingDTO] rather than embedded raw, since
// provider.Pricing has no json tags and would otherwise serialize its fields
// capitalized.
type modelInfoDTO struct {
	ID            string          `json:"id"`
	Provider      string          `json:"provider"`
	DisplayName   string          `json:"displayName"`
	ContextWindow int             `json:"contextWindow,omitempty"`
	MaxOutput     int             `json:"maxOutput,omitempty"`
	Pricing       modelPricingDTO `json:"pricing"`
	Reasoning     bool            `json:"reasoning,omitempty"`
	// Available reports whether the daemon host currently has a usable
	// credential for this model's provider. A remote client cannot see the
	// host's auth state itself, so the daemon stamps it (see handleGoferModels).
	Available bool `json:"available"`
	// Unregistered mirrors [provider.ModelInfo.Unregistered]: the SDK synthesized
	// this record rather than reading it from its registry. When true, the
	// sibling metadata — ContextWindow, MaxOutput, Pricing — is UNKNOWN, not
	// zero. A client must render those as unavailable ("—"), never as a free or
	// already-exhausted model; a percent-of-context gauge divided by a zero
	// ContextWindow is a bug, not a full bar.
	//
	// The polarity is load-bearing, NOT a style choice — do not invert it. With
	// omitempty the key is absent in the common SAFE case (registered, metadata
	// real) and present only in the DANGEROUS one, so a lenient client reading
	// `obj["unregistered"] ?? false` is correct in BOTH branches. The positive
	// spelling (a `pricingKnown`) carries the same information with the opposite
	// failure mode: it would be omitted exactly when metadata IS known, so the
	// same `?? false` default would report every registered model as unknown.
	// Defaults must fall on the safe side of the wire.
	Unregistered bool `json:"unregistered,omitempty"`
}

// modelPricingDTO is the tagged wire projection of [provider.Pricing] (per-Mtok
// USD rates). provider.Pricing carries no json tags, so it is copied field by
// field here rather than embedded, keeping the wire keys camelCase.
type modelPricingDTO struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
}

// toModelInfoDTOs gathers the SDK provider registry and projects it into the
// gofer/models wire shape (see toModelInfoDTOsFrom for the projection itself).
// A registry id whose Lookup fails is skipped defensively (provider.Models()
// only ever yields registered ids).
func toModelInfoDTOs(authed map[string]bool) []modelInfoDTO {
	ids := provider.Models()
	infos := make([]provider.ModelInfo, 0, len(ids))
	for _, id := range ids {
		info, ok := provider.Lookup(id)
		if !ok {
			continue
		}
		infos = append(infos, info)
	}
	return toModelInfoDTOsFrom(infos, authed)
}

// toModelInfoDTOsFrom projects [provider.ModelInfo] records into the
// gofer/models wire shape, sorted by (provider, id) for a stable client-facing
// order (provider.Models() is unordered). authed reports which provider ids the
// daemon host is authenticated for; a model whose provider is absent is marked
// Available:false.
//
// Split out from its sole production caller on purpose: taking the records as a
// parameter is the seam that lets a test drive the projection with a synthesized
// record (notably one carrying Unregistered, which the live registry never
// yields) without mutating the process-wide registry. Inlining it back would
// silently delete that coverage.
func toModelInfoDTOsFrom(infos []provider.ModelInfo, authed map[string]bool) []modelInfoDTO {
	out := make([]modelInfoDTO, 0, len(infos))
	for _, info := range infos {
		out = append(out, modelInfoDTO{
			ID:            info.ID,
			Provider:      info.Provider,
			DisplayName:   modelmeta.DisplayName(info.ID),
			ContextWindow: info.ContextWindow,
			MaxOutput:     info.MaxOutput,
			Pricing: modelPricingDTO{
				Input:      info.Pricing.Input,
				Output:     info.Pricing.Output,
				CacheRead:  info.Pricing.CacheRead,
				CacheWrite: info.Pricing.CacheWrite,
			},
			Reasoning:    info.Reasoning,
			Available:    authed[info.Provider],
			Unregistered: info.Unregistered,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// permissionRequestedParams is the wire shape of a gofer/permission_requested
// notification: a lossless projection of [event.PermissionRequested] plus the
// session id a client needs to attribute it. A client reconstructs the event
// directly from these fields.
type permissionRequestedParams struct {
	SessionID string         `json:"sessionId"`
	ID        string         `json:"id"`
	Tool      string         `json:"tool"`
	Spec      map[string]any `json:"spec,omitempty"`
	Trace     []string       `json:"trace,omitempty"`
}

// permissionResolvedParams is the wire shape of a gofer/permission_resolved
// notification: a lossless projection of [event.PermissionResolved] plus the
// session id.
type permissionResolvedParams struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Verdict   string `json:"verdict"`
	Rule      string `json:"rule,omitempty"`
}

// permissionReplyParams is the inbound params shape of the "permission.reply"
// op (contract #1): {id, verdict, remember?}. It carries no session id — the
// daemon resolves the session from the call id (see handlePermissionReply).
type permissionReplyParams struct {
	ID       string        `json:"id"`
	Verdict  event.Verdict `json:"verdict"`
	Remember bool          `json:"remember,omitempty"`
}

// sessionIDParams is the params shape shared by gofer/kill and gofer/archive.
type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

func decodeSessionIDParams(method string, params json.RawMessage) (sessionIDParams, *rpcError) {
	var req sessionIDParams
	if err := json.Unmarshal(params, &req); err != nil {
		return sessionIDParams{}, invalidParams(err)
	}
	if req.SessionID == "" {
		return sessionIDParams{}, invalidParamsMsg(method + ": sessionId is required")
	}
	return req, nil
}

// setModelParams is the params shape of gofer/set_model: {sessionId, model}.
type setModelParams struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model"`
}

// decodeSetModelParams decodes gofer/set_model's params, rejecting a missing
// sessionId or model locally (a clear invalid-params error) rather than
// forwarding an empty model down to [supervisor.Supervisor.SetModel], whose
// own [supervisor.ErrEmptyModel] would otherwise surface identically but
// with a less specific message.
func decodeSetModelParams(params json.RawMessage) (setModelParams, *rpcError) {
	var req setModelParams
	if err := json.Unmarshal(params, &req); err != nil {
		return setModelParams{}, invalidParams(err)
	}
	if req.SessionID == "" {
		return setModelParams{}, invalidParamsMsg(methodGoferSetModel + ": sessionId is required")
	}
	if req.Model == "" {
		return setModelParams{}, invalidParamsMsg(methodGoferSetModel + ": model is required")
	}
	return req, nil
}

// setEffortParams is the params shape of gofer/set_effort: {sessionId, effort}.
type setEffortParams struct {
	SessionID string `json:"sessionId"`
	Effort    string `json:"effort"`
}

// decodeSetEffortParams decodes gofer/set_effort's params, rejecting only a
// missing sessionId.
//
// Note what it deliberately does NOT reject, unlike [decodeSetModelParams]: an
// empty effort. "" is the SDK's documented "clear the level back to the
// provider's default" ([provider.ValidEffort]), i.e. a legitimate request
// rather than a malformed one, so rejecting it as invalid params would make the
// clear operation unreachable over the wire. Which non-empty strings are levels
// at all is [supervisor.Supervisor.SetEffort]'s call (it restates ValidEffort's
// verdict as [supervisor.ErrInvalidEffort]) — an application error, not a
// params error.
func decodeSetEffortParams(params json.RawMessage) (setEffortParams, *rpcError) {
	var req setEffortParams
	if err := json.Unmarshal(params, &req); err != nil {
		return setEffortParams{}, invalidParams(err)
	}
	if req.SessionID == "" {
		return setEffortParams{}, invalidParamsMsg(methodGoferSetEffort + ": sessionId is required")
	}
	return req, nil
}

// encodeSessionCursor renders a session/list pagination offset as an opaque
// cursor token (base64 of the decimal offset) — opaque so a client never
// depends on its internal shape, only round-trips it via
// [acp.ListSessionsResponse.NextCursor]/[acp.ListSessionsRequest.Cursor].
func encodeSessionCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// decodeSessionCursor is encodeSessionCursor's inverse. An empty cursor
// decodes to offset 0 (first page); a malformed cursor is an error so
// handleSessionList can surface it as invalid params rather than silently
// restarting at page 1.
func decodeSessionCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor: %w", err)
	}
	offset, err := strconv.Atoi(string(raw))
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid cursor: %q", cursor)
	}
	return offset, nil
}

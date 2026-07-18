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

// sessionInfoDTO is the wire shape for the gofer-native roster/ps methods. It
// is a deliberate subset of [supervisor.SessionInfo]: Summary, Pending, and
// Artifacts are reserved-and-always-zero in M2 (see the SessionInfo doc) and
// JournalPath is an on-disk implementation detail, so none of the three cross
// the wire.
type sessionInfoDTO struct {
	ID     string         `json:"id"`
	Title  string         `json:"title,omitempty"`
	Status string         `json:"status"`
	Model  string         `json:"model,omitempty"`
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
}

func toSessionInfoDTO(info supervisor.SessionInfo) sessionInfoDTO {
	return sessionInfoDTO{
		ID:      info.ID,
		Title:   info.Title,
		Status:  info.Status.String(),
		Model:   info.Model,
		Cost:    info.Cost,
		Usage:   info.Usage,
		Queued:  info.Queued,
		Pending: info.Pending,
		Created: info.Created,
		Updated: info.Updated,
		Project: info.Project,
		Live:    info.Live,
		Cwd:     info.Cwd,
	}
}

func toSessionInfoDTOs(infos []supervisor.SessionInfo) []sessionInfoDTO {
	out := make([]sessionInfoDTO, len(infos))
	for i, info := range infos {
		out[i] = toSessionInfoDTO(info)
	}
	return out
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

// toModelInfoDTOs projects the SDK provider registry into the gofer/models
// wire shape, sorted by (provider, id) for a stable client-facing order
// (provider.Models() is unordered). authed reports which provider ids the
// daemon host is authenticated for; a model whose provider is absent is
// marked Available:false. A registry id whose Lookup fails is skipped
// defensively (provider.Models() only ever yields registered ids).
func toModelInfoDTOs(authed map[string]bool) []modelInfoDTO {
	ids := provider.Models()
	out := make([]modelInfoDTO, 0, len(ids))
	for _, id := range ids {
		info, ok := provider.Lookup(id)
		if !ok {
			continue
		}
		out = append(out, modelInfoDTO{
			ID:            info.ID,
			Provider:      info.Provider,
			DisplayName:   modelmeta.DisplayName(id),
			ContextWindow: info.ContextWindow,
			MaxOutput:     info.MaxOutput,
			Pricing: modelPricingDTO{
				Input:      info.Pricing.Input,
				Output:     info.Pricing.Output,
				CacheRead:  info.Pricing.CacheRead,
				CacheWrite: info.Pricing.CacheWrite,
			},
			Reasoning: info.Reasoning,
			Available: authed[info.Provider],
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

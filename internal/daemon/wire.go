package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

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

package daemon

import (
	"encoding/json"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// sessionInfoDTO is the wire shape for the gofer-native roster/ps methods. It
// is a deliberate subset of [supervisor.SessionInfo]: Summary, Pending, and
// Artifacts are reserved-and-always-zero in M2 (see the SessionInfo doc) and
// JournalPath is an on-disk implementation detail, so none of the three cross
// the wire.
type sessionInfoDTO struct {
	ID      string         `json:"id"`
	Title   string         `json:"title,omitempty"`
	Status  string         `json:"status"`
	Model   string         `json:"model,omitempty"`
	Cost    provider.Cost  `json:"cost"`
	Usage   provider.Usage `json:"usage"`
	Queued  int            `json:"queued"`
	Created time.Time      `json:"created"`
	Updated time.Time      `json:"updated"`
	Project string         `json:"project,omitempty"`
	// Live is false for a disk-only (archived/offline) entry from gofer/ps;
	// always true from gofer/roster, which only ever returns live sessions.
	Live bool `json:"live"`
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
		Created: info.Created,
		Updated: info.Updated,
		Project: info.Project,
		Live:    info.Live,
	}
}

func toSessionInfoDTOs(infos []supervisor.SessionInfo) []sessionInfoDTO {
	out := make([]sessionInfoDTO, len(infos))
	for i, info := range infos {
		out[i] = toSessionInfoDTO(info)
	}
	return out
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

// loadSessionResult is the (reserved-empty) result of a session/load request.
// ACP's session/load response carries no fields the client doesn't already
// know (it supplied sessionId in the request); the acp package does not
// export a response type for it (unlike session/new's
// [acp.NewSessionResponse]), so this local type fills that gap per its own
// documented convention.
type loadSessionResult struct{}

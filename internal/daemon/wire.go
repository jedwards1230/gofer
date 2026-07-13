package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
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

package daemon

import (
	"encoding/json"
	"fmt"
)

// jsonrpcVersion is the "jsonrpc" field every envelope carries.
const jsonrpcVersion = "2.0"

// Standard JSON-RPC 2.0 error codes, plus the gofer-owned implementation-defined
// codes (JSON-RPC reserves -32000..-32099 for those): an application-error code
// for daemon/supervisor failures that have no closer standard fit (e.g. "session
// not live"), and the session-cwd-missing code session/load answers with.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeAppError       = -32000

	// codeSessionCwdMissing is what session/load answers when the client asked
	// to resume a session WHERE IT WAS RECORDED — a blank
	// acp.LoadSessionRequest.Cwd, meaning "reopen this session in its own
	// directory" — and that recorded directory no longer resolves to an
	// existing one. It is deliberately NOT codeInvalidParams: the params are
	// fine, the world changed, and only that distinction lets a client offer
	// "pick a new directory" instead of reporting a malformed request. See
	// [sessionCwdMissing] for the payload and [SessionCwdMissing] for the
	// client-side predicate.
	codeSessionCwdMissing = -32001
)

// inboundEnvelope is the shape of any client->daemon frame: a request (method +
// id), a notification (method, no id), a RESPONSE to a daemon-initiated request
// (id + result/error, no method — see [peer.request]), or a malformed message.
// json.RawMessage fields are decoded lazily by the method handler, and ID is
// echoed back verbatim on a response rather than re-encoded, so a client's own
// id type (number or string) round-trips exactly.
type inboundEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Result and Error are set only when the frame is a response to a
	// daemon-initiated request (the daemon acting as a JSON-RPC client on the
	// connection): the client answers a session/request_permission REQUEST with
	// one of these. For an inbound request/notification they are absent.
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// isResponse reports whether e is a response to a daemon-initiated request: it
// has an id, no method, and carries a result or error. A request always names a
// method, so a method-less, id-bearing frame that carries a result/error can
// only be a response.
func (e inboundEnvelope) isResponse() bool {
	return e.Method == "" && len(e.ID) > 0 && (len(e.Result) > 0 || e.Error != nil)
}

// isNotification reports whether e carries no id, per JSON-RPC 2.0 — a
// notification never receives a response, even an error one.
func (e inboundEnvelope) isNotification() bool { return len(e.ID) == 0 }

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// outboundResponse is a JSON-RPC 2.0 response: exactly one of Result or Error
// is set.
type outboundResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// outboundNotification is a JSON-RPC 2.0 notification: a request-shaped
// message with no id, used both for ACP session/update pushes and (were
// gofer ever to need it) daemon-initiated notices.
type outboundNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// outboundRequest is a JSON-RPC 2.0 request the daemon sends TO a client (the
// agent->client direction), for session/request_permission. ID is a raw,
// pre-marshaled id so the daemon owns its own request-id space (see
// [peer.request]); the client echoes it back on the response [peer.deliverReply]
// routes.
type outboundRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

// parseError builds the -32700 error for a frame that failed to parse as
// JSON at all — the id is unknown, so the caller replies with id: null per
// spec.
func parseError(err error) *rpcError {
	return &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()}
}

// invalidRequest builds the -32600 error for a well-formed JSON frame that is
// not a valid JSON-RPC request (e.g. no method).
func invalidRequest(msg string) *rpcError {
	return &rpcError{Code: codeInvalidRequest, Message: msg}
}

// methodNotFound builds the -32601 error for an unregistered method name.
func methodNotFound(method string) *rpcError {
	return &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
}

// invalidParams builds the -32602 error from a params-decoding failure. err's
// message (from acp.DecodeOp/DecodeInitialize or a local json.Unmarshal) is
// already contextual, so it is used verbatim.
func invalidParams(err error) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: err.Error()}
}

// invalidParamsMsg builds the -32602 error from a plain message, for local
// validation failures that have no wrapped error (e.g. a missing required
// field).
func invalidParamsMsg(msg string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: msg}
}

// internalErr builds the -32603 error for a daemon-side bug — e.g. acp.DecodeOp
// reporting ok=true with a type this router does not expect. It should never
// be reachable in practice; it exists so such a drift fails loudly over the
// wire instead of panicking the connection.
func internalErr(err error) *rpcError {
	return &rpcError{Code: codeInternalError, Message: err.Error()}
}

// sessionCwdMissingData is the structured payload [sessionCwdMissing] carries in
// rpcError.Data. The directory travels as a FIELD, never only inside the human
// message: a client reads it back by decoding this struct
// ([SessionCwdMissing]), so nothing on the path ever parses prose to learn which
// directory went missing.
type sessionCwdMissingData struct {
	Cwd string `json:"cwd"`
}

// sessionCwdMissing builds the [codeSessionCwdMissing] error for a session whose
// RECORDED working directory can no longer be resolved. cwd is that recorded
// directory — the one thing a client needs to tell the user what is gone — and
// detail is the underlying resolution failure from [resolveSessionCwd] (deleted,
// not a directory, no longer absolute). See [resolveLoadCwd] for why this is an
// error rather than a fallback.
func sessionCwdMissing(cwd, detail string) *rpcError {
	data, err := json.Marshal(sessionCwdMissingData{Cwd: cwd})
	if err != nil {
		// Unreachable: the payload is a single string field. Reported rather
		// than dropped so a future field that CAN fail to marshal fails loudly.
		return internalErr(fmt.Errorf("session cwd missing: encode error data: %w", err))
	}
	return &rpcError{
		Code:    codeSessionCwdMissing,
		Message: fmt.Sprintf("session cwd %q, recorded when the session was created, is no longer available: %s", cwd, detail),
		Data:    data,
	}
}

// appError builds the -32000 application error from a supervisor/session
// failure. Returns nil for a nil err so handlers can write
// `return result, appError(err)` unconditionally.
func appError(err error) *rpcError {
	if err == nil {
		return nil
	}
	return &rpcError{Code: codeAppError, Message: err.Error()}
}

package mcpconn

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jedwards1230/agent-sdk-go/mcp"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
)

// originalNamer is satisfied by the SDK's unexported mcp.projectedTool —
// exported as a METHOD, not a type, so a structural assertion is the only
// way to reach it. See [projectTools].
type originalNamer interface {
	OriginalName() string
}

// projectTools connects client's tool list through [mcp.Project] (so
// naming/sanitization/schema-conversion stay the SDK's one implementation),
// filters by srv's Allow/Deny, and wraps each survivor as a [proxyTool] that
// resolves ITS CURRENT live client through mgr on every call rather than
// capturing client itself — see the package doc's "Reconnect without
// re-registration".
func projectTools(ctx context.Context, mgr *Manager, client *mcp.Client, srv config.MCPServer) ([]tool.Tool, error) {
	projected, err := mcp.Project(ctx, client, srv.Name)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(projected))
	for _, t := range projected {
		original := t.Name()
		if on, ok := t.(originalNamer); ok {
			original = on.OriginalName()
		}
		if !allowedTool(srv.Allow, srv.Deny, original) {
			continue
		}
		out = append(out, &proxyTool{
			manager:  mgr,
			server:   srv.Name,
			original: original,
			name:     t.Name(),
			desc:     t.Description(),
			schema:   t.Spec(),
		})
	}
	return out, nil
}

// proxyTool implements [tool.Tool] over one MCP server tool, WITHOUT pinning
// itself to the *mcp.Client that happened to be live when it was projected.
// Its identity (Name/Description/Spec) is fixed forever, exactly like the
// SDK's own projectedTool — that is what lets the SAME proxyTool object stay
// registered in a session's tool.Registry across a reconnect (see the
// package doc). Only Run differs: it resolves the CURRENT client through
// manager on every call.
type proxyTool struct {
	manager  *Manager
	server   string
	original string // the server's own tool name — what tools/call is sent
	name     string // qualified name, e.g. "mcp__wiki__read_page"
	desc     string
	schema   tool.Schema
}

func (t *proxyTool) Name() string        { return t.name }
func (t *proxyTool) Description() string { return t.desc }
func (t *proxyTool) Spec() tool.Schema   { return t.schema }

// Run mirrors mcp.projectedTool.Run's resilience contract exactly (see its
// doc): a dead/unreachable server degrades to tool.Result{IsError: true},
// never a Go error, UNLESS ctx itself (the one Run was given) is what ended
// the call — checked via ctx.Err() on the ORIGINAL ctx, after CallTool
// returns its own internally-timeout-derived error, so a genuine turn
// cancellation still aborts the turn like any other tool's would. The one
// difference from the SDK's own implementation: "no live client" (never
// connected, or died and not yet reconnected) is handled BEFORE attempting a
// call at all, since there may be no *mcp.Client to call through.
func (t *proxyTool) Run(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	client := t.manager.live(t.server)
	if client == nil {
		return tool.Result{
			IsError: true,
			Content: fmt.Sprintf("mcp server %q is not connected", t.server),
		}, nil
	}
	res, err := client.CallTool(ctx, t.original, input)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tool.Result{}, ctxErr
		}
		return tool.Result{
			IsError: true,
			Content: fmt.Sprintf("mcp server %s tool %s failed: %v", t.server, t.original, err),
		}, nil
	}
	return tool.Result{IsError: res.IsError, Content: res.Text}, nil
}

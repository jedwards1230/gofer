package mcpconn

// fake_server_internal_test.go is a minimal, hermetic, in-memory MCP server:
// no subprocess, no network. It speaks just enough of the protocol
// (initialize, tools/list, tools/call) over a pair of io.Pipes to drive a
// real agent-sdk-go/mcp.Client end to end — the same seam the SDK's own mcp
// package tests use (mcp.NewStdio wraps an io.ReadWriteCloser; here that
// wrapper is a pipePair rather than a subprocess's stdio).

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/jedwards1230/agent-sdk-go/mcp"
)

// fakeTool is one tool a fakeServer offers.
type fakeTool struct {
	name   string
	desc   string
	schema string // raw JSON Schema object, e.g. `{"type":"object"}`
	call   func(args json.RawMessage) (text string, isError bool)
}

// pipePair is a full-duplex in-memory transport: client writes flow to the
// server's reader, server writes flow to the client's reader.
type pipePair struct {
	c2sR *io.PipeReader
	c2sW *io.PipeWriter
	s2cR *io.PipeReader
	s2cW *io.PipeWriter

	writeMu sync.Mutex // serializes fakeServer's own response writes
}

func newPipePair() *pipePair {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	return &pipePair{c2sR: c2sR, c2sW: c2sW, s2cR: s2cR, s2cW: s2cW}
}

// clientRW is the io.ReadWriteCloser [mcp.NewStdio] wraps for the client
// side of this pipe pair.
func (p *pipePair) clientRW() io.ReadWriteCloser {
	return &rwc{r: p.s2cR, w: p.c2sW}
}

// kill simulates the server process dying: closing the reader end a
// blocked/future client Write targets makes that Write fail immediately
// (io.ErrClosedPipe) instead of hanging, and closing the writer end the
// client reads from delivers it an immediate EOF — exactly what a real
// subprocess's stdin/stdout look like to this client after it exits.
func (p *pipePair) kill() {
	_ = p.c2sR.Close()
	_ = p.s2cW.Close()
}

type rwc struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (x *rwc) Read(b []byte) (int, error)  { return x.r.Read(b) }
func (x *rwc) Write(b []byte) (int, error) { return x.w.Write(b) }
func (x *rwc) Close() error {
	_ = x.r.Close()
	return x.w.Close()
}

// serveFake runs a minimal MCP server over pp until its reader is closed
// (pp.kill, or the client closing its own write side). One request is
// handled at a time — sufficient for these tests' sequential call patterns.
func serveFake(pp *pipePair, tools []fakeTool) {
	r := bufio.NewReader(pp.c2sR)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		var req struct {
			ID     *int64          `json:"id,omitempty"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || req.ID == nil {
			continue // malformed, or a notification (no response expected)
		}

		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"serverInfo": map[string]string{"name": "fake", "version": "0"},
			}
		case "tools/list":
			list := make([]map[string]any, 0, len(tools))
			for _, tl := range tools {
				list = append(list, map[string]any{
					"name": tl.name, "description": tl.desc,
					"inputSchema": json.RawMessage(tl.schema),
				})
			}
			resp["result"] = map[string]any{"tools": list}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			found := false
			for _, tl := range tools {
				if tl.name == params.Name {
					text, isErr := tl.call(params.Arguments)
					resp["result"] = map[string]any{
						"content": []map[string]any{{"type": "text", "text": text}},
						"isError": isErr,
					}
					found = true
					break
				}
			}
			if !found {
				resp["error"] = map[string]any{"code": -32601, "message": "unknown tool"}
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}

		b, merr := json.Marshal(resp)
		if merr != nil {
			return
		}
		pp.writeMu.Lock()
		_, werr := pp.s2cW.Write(append(b, '\n'))
		pp.writeMu.Unlock()
		if werr != nil {
			return
		}
	}
}

// startFakeClient wires a fresh pipePair + serveFake goroutine and returns
// an already-Initialize'd *mcp.Client plus the pair (so the caller can
// pp.kill() it to simulate the server dying).
func startFakeClient(ctx context.Context, tools []fakeTool, opts ...mcp.Option) (*mcp.Client, *pipePair, error) {
	pp := newPipePair()
	go serveFake(pp, tools)
	client := mcp.NewStdio(pp.clientRW(), opts...)
	if _, err := client.Initialize(ctx, mcp.ClientInfo{Name: "test", Version: "0"}); err != nil {
		return nil, pp, err
	}
	return client, pp, nil
}

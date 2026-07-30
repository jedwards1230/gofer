package mcpconn

// dial_internal_test.go covers resolveEnv/resolveHeaders/dialHTTP.
// dialHTTP's own test uses httptest.NewServer — loopback-only, no external
// network dependency — since, unlike stdio, there is no SDK-exposed seam to
// swap an HTTP transport for an in-memory one the way mcp.NewStdio lets
// stdio tests avoid a subprocess.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestResolveEnv(t *testing.T) {
	t.Setenv("MCPCONN_TEST_ENV", "secret-value")
	env, err := resolveEnv(map[string]config.SecretRef{
		"B_VAR": "env:MCPCONN_TEST_ENV",
		"A_VAR": "env:MCPCONN_TEST_ENV",
	})
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	want := []string{"A_VAR=secret-value", "B_VAR=secret-value"} // sorted by key
	if len(env) != len(want) || env[0] != want[0] || env[1] != want[1] {
		t.Fatalf("resolveEnv = %v, want %v (sorted, resolved)", env, want)
	}
}

func TestResolveEnv_UnresolvableSecretErrors(t *testing.T) {
	_ = os.Unsetenv("MCPCONN_TEST_ENV_MISSING")
	_, err := resolveEnv(map[string]config.SecretRef{"K": "env:MCPCONN_TEST_ENV_MISSING"})
	if err == nil {
		t.Fatal("resolveEnv: want an error for an unset env: secret ref")
	}
}

// TestDialHTTP_HeadersAndAuth proves dialHTTP resolves srv.Headers and
// srv.Auth into the request the server actually sees, and that an explicit
// "Authorization" header wins over Auth's bearer-token default.
func TestDialHTTP_HeadersAndAuth(t *testing.T) {
	t.Setenv("MCPCONN_TEST_TOKEN", "tok-123")

	var gotAuth, gotCustom string
	srvHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Custom")
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{"serverInfo":{"name":"fake","version":"0"}}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{}}`))
		}
	}))
	defer srvHTTP.Close()

	srv := config.MCPServer{
		Name:    "http-fake",
		URL:     srvHTTP.URL,
		Headers: map[string]config.SecretRef{"X-Custom": "env:MCPCONN_TEST_TOKEN"},
		Auth:    "env:MCPCONN_TEST_TOKEN",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := dialHTTP(ctx, srv, time.Second, time.Second)
	if err != nil {
		t.Fatalf("dialHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization header = %q, want %q (Auth as bearer, no explicit Authorization header set)", gotAuth, "Bearer tok-123")
	}
	if gotCustom != "tok-123" {
		t.Fatalf("X-Custom header = %q, want %q", gotCustom, "tok-123")
	}
}

// TestDialHTTP_ExplicitAuthorizationHeaderWins proves Headers["Authorization"]
// overrides the Auth-as-bearer default rather than the two colliding.
func TestDialHTTP_ExplicitAuthorizationHeaderWins(t *testing.T) {
	t.Setenv("MCPCONN_TEST_TOKEN2", "should-not-be-used")

	var gotAuth string
	srvHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":{"serverInfo":{"name":"fake","version":"0"}}}`))
	}))
	defer srvHTTP.Close()

	srv := config.MCPServer{
		Name:    "http-fake",
		URL:     srvHTTP.URL,
		Headers: map[string]config.SecretRef{"Authorization": "env:MCPCONN_TEST_TOKEN2"},
		Auth:    "env:MCPCONN_TEST_TOKEN2",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := dialHTTP(ctx, srv, time.Second, time.Second)
	if err != nil {
		t.Fatalf("dialHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	if gotAuth != "should-not-be-used" {
		t.Fatalf("Authorization header = %q, want the explicit Headers value verbatim (no \"Bearer \" prefix added)", gotAuth)
	}
	if strings.Contains(gotAuth, "Bearer") {
		t.Fatalf("Authorization header = %q, Auth's bearer default must not also apply", gotAuth)
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

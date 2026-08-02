package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// fauxProvider returns a provider.Provider constructor for newTestSupervisor
// that replays faux.Default() — one scripted turn: brief reasoning, then a
// short greeting, stop reason end_turn.
func fauxProvider() provider.Provider { return faux.New(faux.Default()) }

// TestInitializeHandshake covers the initialize round trip: the daemon echoes
// [acp.ProtocolVersion] regardless of what the client requests.
func TestInitializeHandshake(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c := dial(t, context.Background(), url, nil)
	resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	var got acp.InitializeResponse
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal InitializeResponse: %v", err)
	}
	if got.ProtocolVersion != acp.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", got.ProtocolVersion, acp.ProtocolVersion)
	}
}

// TestAuthenticateNoop covers the authenticate handshake: a no-op success,
// since the WebSocket bearer token is the daemon's only M2 auth boundary.
func TestAuthenticateNoop(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c := dial(t, context.Background(), url, nil)
	resp := c.request(acp.MethodAuthenticate, struct{}{})
	if resp.Error != nil {
		t.Fatalf("authenticate error: %+v", resp.Error)
	}
}

// TestUnknownMethod asserts an unregistered method name gets -32601.
func TestUnknownMethod(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c := dial(t, context.Background(), url, nil)
	resp := c.request("bogus/method", nil)
	if resp.Error == nil {
		t.Fatal("bogus/method: want an error, got none")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

// TestMalformedFrame asserts a frame that isn't valid JSON gets a -32700
// parse error with a null id.
func TestMalformedFrame(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c := dial(t, context.Background(), url, nil)
	c.writeRaw(`{not valid json`)
	resp := c.waitRawResponse()
	if resp.Error == nil {
		t.Fatal("malformed frame: want an error, got none")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700", resp.Error.Code)
	}
	if string(resp.ID) != "null" {
		t.Errorf("id = %s, want null", resp.ID)
	}
}

// TestBearerAuth_Rejected asserts a missing or wrong bearer token is refused
// with 401 before the connection is ever upgraded.
func TestBearerAuth_Rejected(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "the-right-token")
	httpURL := "http" + url[len("ws"):]

	cases := []struct {
		name   string
		header http.Header
		query  string
	}{
		{name: "no token"},
		{name: "wrong token header", header: http.Header{"Authorization": {"Bearer nope"}}},
		{name: "wrong token query", query: "?token=nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, httpURL+tc.query, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			for k, vs := range tc.header {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}

// TestBearerAuth_Accepted asserts the correct token — via either the
// Authorization header or the token query parameter — upgrades the
// connection and lets a real request through.
func TestBearerAuth_Accepted(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "the-right-token")

	t.Run("header", func(t *testing.T) {
		c := dial(t, context.Background(), url, map[string][]string{"Authorization": {"Bearer the-right-token"}})
		resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
		if resp.Error != nil {
			t.Fatalf("initialize error: %+v", resp.Error)
		}
	})

	t.Run("query param", func(t *testing.T) {
		c := dial(t, context.Background(), url+"?token=the-right-token", nil)
		resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
		if resp.Error != nil {
			t.Fatalf("initialize error: %+v", resp.Error)
		}
	})
}

// TestBearerAuth_Disabled asserts an empty BearerToken accepts every
// connection with no credential at all.
func TestBearerAuth_Disabled(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c := dial(t, context.Background(), url, nil)
	resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
}

// TestExtraTokensAccepted covers [daemon.Config.ExtraTokens]: a credential the
// EMBEDDER mints (today the router's worker dial-back) is accepted alongside the
// operator's bearer token, and a wrong one still is not.
//
// It exists so the router can stop handing workers the operator's own token.
// Both directions are asserted — an accepted-token list that accepted
// everything would satisfy the positive case alone, and one that accepted
// nothing would leave every dial-back failing with an opaque 401.
func TestExtraTokensAccepted(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemonWithConfig(t, sup, daemon.Config{
		BearerToken:  "the-operators-token",
		ExtraTokens:  []string{"the-minted-dial-back-token"},
		DefaultModel: "faux",
	})

	t.Run("the operator's own token still works", func(t *testing.T) {
		c := dial(t, context.Background(), url, map[string][]string{"Authorization": {"Bearer the-operators-token"}})
		if resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion}); resp.Error != nil {
			t.Fatalf("initialize error: %+v", resp.Error)
		}
	})

	t.Run("a minted extra token is accepted", func(t *testing.T) {
		c := dial(t, context.Background(), url, map[string][]string{"Authorization": {"Bearer the-minted-dial-back-token"}})
		if resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion}); resp.Error != nil {
			t.Fatalf("initialize error: %+v", resp.Error)
		}
	})

	t.Run("an unknown token is still refused", func(t *testing.T) {
		httpURL := "http" + url[len("ws"):]
		req, err := http.NewRequest(http.MethodGet, httpURL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer neither-of-them")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d — adding an accepted-token list must not widen auth to everything",
				resp.StatusCode, http.StatusUnauthorized)
		}
	})
}

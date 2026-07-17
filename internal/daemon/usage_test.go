package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// usageProvider is a scripted [provider.Provider] whose single turn emits one
// assistant text delta and then a StreamFinished carrying genuine token usage.
// Paired with a registered model id (see TestSessionPromptUsageUpdate), its
// TurnFinished event is priced and stamped with the model's context window by
// the SDK loop — the two conditions acp.ToSessionUpdate requires to project a
// usage_update. The faux provider can't reach this path (its "faux" model is
// unregistered, so ContextWindow stays 0), which is why this test scripts its
// own provider rather than reusing fauxProvider.
type usageProvider struct{ usage provider.Usage }

func (usageProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: usageModelID} }

func (p usageProvider) Stream(context.Context, provider.Request) (provider.StreamHandle, error) {
	return &usageStream{usage: p.usage}, nil
}

type usageStream struct {
	usage provider.Usage
	n     int
}

func (s *usageStream) Next() (provider.StreamEvent, error) {
	s.n++
	switch s.n {
	case 1:
		return provider.StreamEvent{Type: provider.StreamTextDelta, Text: "done"}, nil
	case 2:
		// The terminal event carries the settled stop reason and usage. The SDK
		// loop prices this usage and stamps the model's context window onto the
		// resulting TurnFinished (see loop.runner.turnFinished).
		return provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: s.usage}, nil
	default:
		return provider.StreamEvent{}, io.EOF
	}
}

func (s *usageStream) Close() error { return nil }

// usageModelID is a model registered in the SDK's provider registry, so
// provider.Lookup / provider.CostOf resolve a context window and pricing for it
// — unlike the "faux" model the other daemon tests use.
const usageModelID = "claude-opus-4-8"

// usageUpdateParams is the wire shape of a usage_update session/update
// notification's params: the discriminator plus the token counters and (when
// the model is priced) the cost. It mirrors the SDK's UsageUpdate JSON
// (acp.UsageUpdate.MarshalJSON), decoded loosely here for the same reason
// sessionUpdateParams is — acp has no client-direction decode helpers.
type usageUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Used          uint64 `json:"used"`
		Size          uint64 `json:"size"`
		Cost          *struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		} `json:"cost"`
	} `json:"update"`
}

// TestSessionPromptUsageUpdate proves an ACP usage_update crosses gofer's
// session/update wire. It drives a real session/prompt turn through the full
// daemon projection path (acp.ToSessionUpdate -> broadcastUpdate -> peer
// notify) with a registered model and a provider that reports token usage, then
// asserts the peer receives a "usage_update" notification — carrying used/size
// and a priced cost — BEFORE the terminal PromptResponse. gofer itself has no
// usage-update code: the variant lights up purely by the SDK bump, because
// gofer's session/update stream is a pass-through of whatever ToSessionUpdate
// returns ok=true for.
func TestSessionPromptUsageUpdate(t *testing.T) {
	usage := provider.Usage{InputTokens: 1000, CacheReadTokens: 200, OutputTokens: 300}
	sup := newTestSupervisorModelAtRoot(t, t.TempDir(), usageModelID, func() provider.Provider {
		return usageProvider{usage: usage}
	})
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()

	// The turn streams one assistant text delta (agent_message_chunk) and then a
	// TurnFinished that projects to usage_update — in that order, both before the
	// prompt response resolves. Scan the notification stream for the usage_update.
	var usageNotif *usageUpdateParams
	for usageNotif == nil {
		n := c.waitNotification()
		if n.Method != acp.MethodSessionUpdate {
			t.Fatalf("notification method = %q, want %q", n.Method, acp.MethodSessionUpdate)
		}
		var up usageUpdateParams
		if err := json.Unmarshal(n.Params, &up); err != nil {
			t.Fatalf("unmarshal usage_update params: %v", err)
		}
		if up.Update.SessionUpdate == "usage_update" {
			usageNotif = &up
		}
	}

	if usageNotif.SessionID != sid {
		t.Errorf("usage_update sessionId = %q, want %q", usageNotif.SessionID, sid)
	}
	// used = input + cache-read + output = 1000 + 200 + 300.
	if got, want := usageNotif.Update.Used, uint64(1500); got != want {
		t.Errorf("usage_update used = %d, want %d", got, want)
	}
	if got, want := usageNotif.Update.Size, uint64(1_000_000); got != want {
		t.Errorf("usage_update size = %d, want %d (context window of %s)", got, want, usageModelID)
	}
	if usageNotif.Update.Cost == nil {
		t.Fatal("usage_update cost is nil, want a priced cost (model is in the registry)")
	}
	if usageNotif.Update.Cost.Currency != "USD" {
		t.Errorf("usage_update cost.currency = %q, want %q", usageNotif.Update.Cost.Currency, "USD")
	}
	if usageNotif.Update.Cost.Amount <= 0 {
		t.Errorf("usage_update cost.amount = %v, want > 0", usageNotif.Update.Cost.Amount)
	}

	// The usage_update was delivered before the prompt response resolved: only
	// now do we drain the terminal PromptResponse.
	final := <-respCh
	if final.Error != nil {
		t.Fatalf("session/prompt error: %+v", final.Error)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(final.Result, &pr); err != nil {
		t.Fatalf("unmarshal PromptResponse: %v", err)
	}
	if pr.StopReason != acp.StopReasonEndTurn {
		t.Errorf("StopReason = %q, want %q", pr.StopReason, acp.StopReasonEndTurn)
	}
}

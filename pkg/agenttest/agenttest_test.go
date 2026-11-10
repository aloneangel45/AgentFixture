package agenttest_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/agenttest"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

func TestAgentTestSDK_RecordAndZeroNetworkReplay(t *testing.T) {
	casPath := filepath.Join(t.TempDir(), "sdk_anthropic_sse.yaml")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_sdk_1\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"SDK deterministic stream!\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))

	// Subtest 1: Record into casPath automatically on t.Cleanup
	t.Run("record", func(t *testing.T) {
		ts := agenttest.NewTestServer(t, casPath, agenttest.WithRecordMode(upstream.URL))
		resp, err := ts.HTTPClient().Post(ts.URL()+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-sonnet-4-20250514","stream":true}`))
		if err != nil {
			t.Fatalf("record post failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "SDK deterministic stream!") {
			t.Fatalf("unexpected live stream body: %s", string(body))
		}
	})

	// Shut down upstream completely to prove zero external dependency
	upstream.Close()

	// Subtest 2: Replay with strict exhaustive check + verify zero-outbound-network HTTPClient
	t.Run("replay_offline", func(t *testing.T) {
		ts := agenttest.NewTestServer(t, casPath, agenttest.WithStrictExhaustive(true))
		client := ts.HTTPClient()

		resp, err := client.Post(ts.URL()+"/v1/messages", "application/json", strings.NewReader(`{"stream":true,"model":"claude-sonnet-4-20250514"}`))
		if err != nil {
			t.Fatalf("replay post failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "SDK deterministic stream!") {
			t.Fatalf("unexpected replayed stream body: %s", string(body))
		}

		// Verify outbound external connection via direct offline dialer is strictly blocked
		rawOffline := proxy.NewOfflineHTTPClient("")
		if _, outErr := rawOffline.Get("http://1.1.1.1"); !errors.Is(outErr, proxy.ErrOutboundNetworkBlocked) {
			t.Fatalf("expected outbound IP dial blocked with ErrOutboundNetworkBlocked, got: %v", outErr)
		}
	})
}

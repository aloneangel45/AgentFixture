package matcher

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
)

func BenchmarkStrictMatcher_JSON(b *testing.B) {
	strict := NewStrictMatcher()
	req1 := &cassette.Request{
		Method:   "POST",
		Endpoint: "https://api.anthropic.com/v1/messages?beta=true",
		RawBody:  `{"model":"claude-sonnet-4-20250514","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Analyze pkg/proxy/http_proxy.go for lock contention"}]}`,
	}
	req1.Matcher.StrictHash = ComputeRequestHash(req1)

	req2 := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages?beta=true",
		RawBody:  `{"stream":true,"messages":[{"content":"Analyze pkg/proxy/http_proxy.go for lock contention","role":"user"}],"max_tokens":1024,"model":"claude-sonnet-4-20250514"}`,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := strict.Match(req1, req2)
		if !res.Matched {
			b.Fatal("expected match")
		}
	}
}

func BenchmarkFuzzyMatcher_ShortPrompt(b *testing.B) {
	fuzzy := NewFuzzyMatcher(0.80)
	req1 := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"model":"claude-sonnet-4-20250514","request_id":"req_111","prompt":"Inspect auth middleware at 2026-11-10T10:00:00Z"}`,
	}
	req2 := &cassette.Request{
		Method:   "POST",
		Endpoint: "/v1/messages",
		RawBody:  `{"model":"claude-sonnet-4-20250514","request_id":"req_222","prompt":"Inspect auth middleware at 2026-11-11T15:30:00Z"}`,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := fuzzy.Match(req1, req2)
		if !res.Matched {
			b.Fatal("expected match")
		}
	}
}

func BenchmarkFuzzyMatcher_100KBPrompt_MiddleEdit(b *testing.B) {
	fuzzy := NewFuzzyMatcher(0.80)
	var sb1, sb2 strings.Builder
	for i := 0; i < 1600; i++ {
		line := fmt.Sprintf("Line %04d: package systems_architecture_context_block_%d_payload_data\n", i, i%97)
		sb1.WriteString(line)
		if i%50 == 0 {
			sb2.WriteString(fmt.Sprintf("Line %04d: package systems_architecture_MODIFIED_block_%d_payload_data\n", i, i%97))
		} else {
			sb2.WriteString(line)
		}
	}
	p1 := sb1.String() // ~110 KB
	p2 := sb2.String() // ~110 KB with edits across the entire 100KB interior

	req1 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: p1}
	req2 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: p2}

	b.SetBytes(int64(len(p1) + len(p2)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := fuzzy.Match(req1, req2)
		if !res.Matched {
			b.Fatalf("expected match on 100KB similar prompt, got score=%.3f", res.Score)
		}
	}
}

func BenchmarkFuzzyMatcher_100KBDisjointPrompt(b *testing.B) {
	fuzzy := NewFuzzyMatcher(0.80)
	p1 := strings.Repeat("alpha_beta_gamma_delta_12345 ", 3600)  // ~104 KB
	p2 := strings.Repeat("omega_sigma_theta_kappa_98765 ", 3600) // ~108 KB

	req1 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: p1}
	req2 := &cassette.Request{Method: "POST", Endpoint: "/v1/messages", RawBody: p2}

	b.SetBytes(int64(len(p1) + len(p2)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := fuzzy.Match(req1, req2)
		if res.Matched {
			b.Fatalf("expected mismatch on disjoint 100KB prompts, got score=%.3f", res.Score)
		}
	}
}

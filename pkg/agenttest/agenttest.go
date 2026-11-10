// Package agenttest provides an ergonomic Go testing SDK for zero-dollar, zero-network
// deterministic VCR regression testing of LLM and tool-using agents.
package agenttest

import (
	"net/http"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
	"github.com/aloneangel45/AgentFixture/pkg/proxy"
)

// Option configures a TestServer instance.
type Option = proxy.TestOption

// TestServer manages a local loopback VCR server bound to testing.TB.
type TestServer struct {
	inner *proxy.TestServer
}

// WithMatcher overrides the request Matcher (default: StrictMatcher).
func WithMatcher(m matcher.Matcher) Option {
	return proxy.WithTestMatcher(m)
}

// WithFuzzyMatcher configures semantic fuzzy matching with the given threshold (e.g. 0.85).
func WithFuzzyMatcher(threshold float64) Option {
	return proxy.WithTestMatcher(matcher.NewFuzzyMatcher(threshold))
}

// WithSSETiming configures chunk pacing for recorded SSE streams (instant, scaled, or exact).
func WithSSETiming(cfg cassette.SSETimingConfig) Option {
	return proxy.WithTestSSETiming(cfg)
}

// WithStrictExhaustive asserts on tb.Cleanup that every interaction in the cassette was replayed.
func WithStrictExhaustive(strict bool) Option {
	return proxy.WithTestStrictExhaustive(strict)
}

// WithAllowUnmatched suppresses automatic tb.Cleanup errors when negative tests intentionally send unmatched requests.
func WithAllowUnmatched(allow bool) Option {
	return proxy.WithTestAllowUnmatched(allow)
}

// WithRecordMode switches the TestServer into live recording mode against upstreamURL and writes cassettePath on cleanup.
func WithRecordMode(upstreamURL string) Option {
	return proxy.WithTestRecordMode(upstreamURL)
}

// NewTestServer loads cassettePath, starts a local replay proxy on 127.0.0.1:0,
// provides a zero-outbound-network HTTPClient(), and registers automatic tb.Cleanup verification.
func NewTestServer(tb testing.TB, cassettePath string, opts ...Option) *TestServer {
	tb.Helper()
	return &TestServer{
		inner: proxy.NewTestServer(tb, cassettePath, opts...),
	}
}

// URL returns the local base URL (e.g. "http://127.0.0.1:54321") suitable for ANTHROPIC_BASE_URL or OPENAI_BASE_URL.
func (ts *TestServer) URL() string {
	return ts.inner.URL()
}

// HTTPClient returns a zero-network *http.Client whose DialContext blocks any non-loopback connection.
func (ts *TestServer) HTTPClient() *http.Client {
	return ts.inner.HTTPClient()
}

// Server returns the underlying *proxy.Server.
func (ts *TestServer) Server() *proxy.Server {
	return ts.inner.Server()
}

// Cassette returns a snapshot of the loaded or recorded Cassette.
func (ts *TestServer) Cassette() *cassette.Cassette {
	return ts.inner.Server().GetCassette()
}

// AssertAllMatched verifies that every interaction in the cassette has been replayed at least once.
func (ts *TestServer) AssertAllMatched() {
	ts.inner.AssertAllMatched()
}

package proxy

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/chaos"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// TestOption configures a VCR TestServer created via NewTestServer.
type TestOption func(*testServerConfig)

type testServerConfig struct {
	matcher          matcher.Matcher
	sseTiming        cassette.SSETimingConfig
	strictAllMatched bool
	allowUnmatched   bool
	mode             Mode
	recordMode       RecordMode
	upstreamURL      string
	chaos            *chaos.Injector
}

// WithTestMatcher overrides the request Matcher (default: StrictMatcher).
func WithTestMatcher(m matcher.Matcher) TestOption {
	return func(c *testServerConfig) {
		c.matcher = m
	}
}

// WithTestSSETiming sets the SSE chunk replay timing configuration.
func WithTestSSETiming(cfg cassette.SSETimingConfig) TestOption {
	return func(c *testServerConfig) {
		c.sseTiming = cfg
	}
}

// WithTestStrictExhaustive fails tb.Cleanup if any cassette interaction was not exercised.
func WithTestStrictExhaustive(strict bool) TestOption {
	return func(c *testServerConfig) {
		c.strictAllMatched = strict
	}
}

// WithTestAllowUnmatched disables automatic tb.Cleanup failure when unexpected requests arrive.
func WithTestAllowUnmatched(allow bool) TestOption {
	return func(c *testServerConfig) {
		c.allowUnmatched = allow
	}
}

// WithTestRecordMode switches the TestServer into live recording mode against upstreamURL
// and automatically saves the cassette on tb.Cleanup.
func WithTestRecordMode(upstreamURL string) TestOption {
	return func(c *testServerConfig) {
		c.mode = ModeRecord
		c.recordMode = RecordModeAll
		c.upstreamURL = upstreamURL
	}
}

// WithTestHybridRecordMode configures hybrid recording (none, new_episodes, all) against upstreamURL.
func WithTestHybridRecordMode(mode RecordMode, upstreamURL string) TestOption {
	return func(c *testServerConfig) {
		c.recordMode = mode
		c.upstreamURL = upstreamURL
		if mode == RecordModeAll {
			c.mode = ModeRecord
		} else {
			c.mode = ModeReplay
		}
	}
}

// WithTestChaos attaches a deterministic chaos.Injector to the TestServer.
func WithTestChaos(inj *chaos.Injector) TestOption {
	return func(c *testServerConfig) {
		c.chaos = inj
	}
}

// TestServer wraps a deterministic AgentFixture VCR Server bound to a Go testing.TB lifecycle.
type TestServer struct {
	tb     testing.TB
	path   string
	srv    *Server
	client *http.Client
	cfg    testServerConfig
}

// NewTestServer starts a zero-network deterministic VCR server for Go unit and integration tests.
func NewTestServer(tb testing.TB, cassettePath string, opts ...TestOption) *TestServer {
	tb.Helper()

	cfg := testServerConfig{
		matcher: matcher.NewStrictMatcher(),
		mode:    ModeReplay,
		sseTiming: cassette.SSETimingConfig{
			Mode: cassette.SSETimingInstant,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	var cas *cassette.Cassette
	var err error
	if cfg.mode == ModeRecord || cfg.recordMode == RecordModeAll {
		cas = cassette.NewCassette(tb.Name())
		cas.Metadata.Source = "agentfixture_go_sdk"
	} else if cfg.recordMode == RecordModeNewEpisodes {
		cas, err = cassette.Load(cassettePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				cas = cassette.NewCassette(tb.Name())
				cas.Metadata.Source = "agentfixture_go_sdk"
			} else {
				tb.Fatalf("agentfixture: failed to load cassette %q: %v", cassettePath, err)
			}
		}
	} else {
		cas, err = cassette.Load(cassettePath)
		if err != nil {
			tb.Fatalf("agentfixture: failed to load cassette %q: %v", cassettePath, err)
		}
	}

	srv, err := NewServer(Config{
		Mode:        cfg.mode,
		RecordMode:  cfg.recordMode,
		UpstreamURL: cfg.upstreamURL,
		Cassette:    cas,
		Matcher:     cfg.matcher,
		SSETiming:   cfg.sseTiming,
		Chaos:       cfg.chaos,
	})
	if err != nil {
		tb.Fatalf("agentfixture: failed to initialize test server: %v", err)
	}
	if err := srv.Start(); err != nil {
		tb.Fatalf("agentfixture: failed to start test server: %v", err)
	}

	ts := &TestServer{
		tb:     tb,
		path:   cassettePath,
		srv:    srv,
		client: NewOfflineHTTPClient(srv.URL()),
		cfg:    cfg,
	}

	tb.Cleanup(func() {
		_ = srv.Close()
		if (cfg.mode == ModeRecord || cfg.recordMode == RecordModeAll) && cassettePath != "" {
			if saveErr := cassette.Save(cassettePath, srv.GetCassette()); saveErr != nil {
				tb.Errorf("agentfixture: failed to save recorded cassette to %q: %v", cassettePath, saveErr)
			}
			return
		}
		if cfg.recordMode == RecordModeNewEpisodes && cassettePath != "" && srv.NewEpisodesCount() > 0 {
			if saveErr := cassette.Save(cassettePath, srv.GetCassette()); saveErr != nil {
				tb.Errorf("agentfixture: failed to save updated cassette to %q: %v", cassettePath, saveErr)
			}
		}
		if tb.Failed() {
			return
		}
		if !cfg.allowUnmatched {
			unmatched := srv.UnmatchedRequests()
			if len(unmatched) > 0 {
				tb.Errorf("agentfixture: %d unexpected request(s) failed to match cassette %q (first: %s %s)",
					len(unmatched), cassettePath, unmatched[0].Method, unmatched[0].Endpoint)
			}
		}
		if cfg.strictAllMatched {
			ts.AssertAllMatched()
		}
	})

	return ts
}

// URL returns the base loopback URL (e.g. "http://127.0.0.1:54321") for ANTHROPIC_BASE_URL / OPENAI_BASE_URL.
func (ts *TestServer) URL() string {
	return ts.srv.URL()
}

// HTTPClient returns a zero-outbound-network *http.Client locked strictly to loopback dials.
func (ts *TestServer) HTTPClient() *http.Client {
	return ts.client
}

// Server exposes the underlying *Server instance.
func (ts *TestServer) Server() *Server {
	return ts.srv
}

// AssertAllMatched fails the test if any recorded interaction in the cassette went uncalled.
func (ts *TestServer) AssertAllMatched() {
	ts.tb.Helper()
	cas := ts.srv.GetCassette()
	matched := ts.srv.MatchedIDs()
	for _, inter := range cas.Interactions {
		if !matched[inter.ID] {
			ts.tb.Errorf("agentfixture: expected cassette interaction %q (%s %s) was never called",
				inter.ID, inter.Request.Method, inter.Request.Endpoint)
		}
	}
}

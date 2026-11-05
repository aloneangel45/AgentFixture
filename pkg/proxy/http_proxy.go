package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// Mode determines proxy runtime behavior.
type Mode string

const (
	ModeReplay      Mode = "replay"
	ModeRecord      Mode = "record"
	ModePassthrough Mode = "passthrough"
)

// Config configures the HTTP proxy/replay server.
type Config struct {
	Mode           Mode
	UpstreamURL    string
	Cassette       *cassette.Cassette
	Matcher        matcher.Matcher
	SimulateDelay  bool
	ListenAddr     string
	MaxReplayDelay time.Duration
}

// Server provides deterministic HTTP playback and recording interception.
type Server struct {
	cfg        Config
	listener   net.Listener
	httpServer *http.Server
	mu         sync.Mutex
	cassette   *cassette.Cassette
	matchedIDs map[string]bool
	unmatched  []cassette.Request
	upstream   *url.URL
}

// NewServer initializes a proxy Server with the given configuration.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeReplay
	}
	if cfg.Matcher == nil {
		cfg.Matcher = matcher.NewStrictMatcher()
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.MaxReplayDelay == 0 {
		cfg.MaxReplayDelay = 50 * time.Millisecond
	}

	var upstreamParsed *url.URL
	var err error
	if cfg.UpstreamURL != "" {
		upstreamParsed, err = url.Parse(cfg.UpstreamURL)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream URL %s: %w", cfg.UpstreamURL, err)
		}
	}

	cas := cfg.Cassette
	if cas == nil {
		cas = cassette.NewCassette(fmt.Sprintf("cas_%d", time.Now().UnixNano()))
	}

	s := &Server{
		cfg:        cfg,
		cassette:   cas,
		matchedIDs: make(map[string]bool),
		unmatched:  make([]cassette.Request, 0),
		upstream:   upstreamParsed,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)
	s.httpServer = &http.Server{Handler: mux}

	return s, nil
}

// Start opens the network listener and begins serving requests in a background goroutine.
func (s *Server) Start() error {
	l, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to bind proxy listener on %s: %w", s.cfg.ListenAddr, err)
	}
	s.listener = l

	go func() {
		_ = s.httpServer.Serve(l)
	}()

	return nil
}

// Close shuts down the server and releases all network ports.
func (s *Server) Close() error {
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// Addr returns the network address string (e.g. 127.0.0.1:54321).
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// URL returns the base HTTP URL pointing to this proxy.
func (s *Server) URL() string {
	return "http://" + s.Addr()
}

// GetCassette returns the in-memory Cassette containing recorded or loaded interactions.
func (s *Server) GetCassette() *cassette.Cassette {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cassette
}

// MatchedCount returns how many cassette interactions have been replayed.
func (s *Server) MatchedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.matchedIDs)
}

// UnmatchedRequests returns any requests received during replay that failed to match.
func (s *Server) UnmatchedRequests() []cassette.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]cassette.Request, len(s.unmatched))
	copy(copied, s.unmatched)
	return copied
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	var jsonBody interface{}
	if len(bodyBytes) > 0 && strings.Contains(r.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(bodyBytes, &jsonBody)
	}

	candidate := cassette.Request{
		Method:   r.Method,
		Endpoint: r.URL.Path,
		Headers:  headers,
		Body:     jsonBody,
		RawBody:  string(bodyBytes),
	}
	candidate.Matcher.StrictHash = matcher.ComputeRequestHash(&candidate)

	switch s.cfg.Mode {
	case ModeReplay:
		s.handleReplay(w, r, &candidate)
	case ModeRecord:
		s.handleRecord(w, r, &candidate, bodyBytes)
	case ModePassthrough:
		s.handlePassthrough(w, r, bodyBytes)
	default:
		http.Error(w, "Unsupported proxy mode", http.StatusInternalServerError)
	}
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request, candidate *cassette.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var bestInter *cassette.Interaction
	var bestScore float64
	var lastDiff string

	for i := range s.cassette.Interactions {
		inter := &s.cassette.Interactions[i]
		res := s.cfg.Matcher.Match(&inter.Request, candidate)
		if res.Matched && res.Score > bestScore {
			bestScore = res.Score
			bestInter = inter
			if res.Score == 1.0 {
				break
			}
		}
		if res.Diff != "" {
			lastDiff = res.Diff
		}
	}

	if bestInter != nil {
		s.matchedIDs[bestInter.ID] = true

		if s.cfg.SimulateDelay && bestInter.Response.LatencyMs > 0 {
			sleepDur := time.Duration(bestInter.Response.LatencyMs) * time.Millisecond
			if sleepDur > s.cfg.MaxReplayDelay {
				sleepDur = s.cfg.MaxReplayDelay
			}
			time.Sleep(sleepDur)
		}

		for k, v := range bestInter.Response.Headers {
			if !isHopByHopHeader(k) {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("X-AgentFixture-Replay", "true")
		w.Header().Set("X-AgentFixture-Interaction-ID", bestInter.ID)

		status := bestInter.Response.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)

		if bestInter.Response.RawBody != "" {
			_, _ = w.Write([]byte(bestInter.Response.RawBody))
		} else if bestInter.Response.ToolResult != "" {
			_, _ = w.Write([]byte(bestInter.Response.ToolResult))
		} else if bestInter.Response.Body != nil {
			_ = json.NewEncoder(w).Encode(bestInter.Response.Body)
		}
		return
	}

	// Mismatch encountered
	s.unmatched = append(s.unmatched, *candidate)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-AgentFixture-Mismatch", "true")
	w.WriteHeader(http.StatusNotFound)

	resp := map[string]interface{}{
		"error":             "AgentFixture replay mismatch: no recorded interaction matched request",
		"candidate_method":   candidate.Method,
		"candidate_endpoint": candidate.Endpoint,
		"diff_explanation":   lastDiff,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request, candidate *cassette.Request, bodyBytes []byte) {
	if s.upstream == nil {
		http.Error(w, "Proxy record mode requires an upstream target URL", http.StatusBadGateway)
		return
	}

	outURL := *s.upstream
	outURL.Path = r.URL.Path
	outURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequest(r.Method, outURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to construct upstream request: %v", err), http.StatusInternalServerError)
		return
	}

	for k, vv := range r.Header {
		if !isHopByHopHeader(k) {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}
	}

	start := time.Now()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(outReq)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read upstream response body: %v", err), http.StatusBadGateway)
		return
	}

	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = strings.Join(v, ", ")
		}
	}

	var respJSON interface{}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(respBody, &respJSON)
	}

	s.mu.Lock()
	seq := len(s.cassette.Interactions) + 1
	inter := cassette.Interaction{
		ID:       fmt.Sprintf("int_%03d", seq),
		Sequence: seq,
		Type:     cassette.InteractionTypeHTTP,
		Request:  *candidate,
		Response: cassette.Response{
			StatusCode: resp.StatusCode,
			LatencyMs:  duration,
			Headers:    respHeaders,
			Body:       respJSON,
			RawBody:    string(respBody),
		},
	}
	s.cassette.Interactions = append(s.cassette.Interactions, inter)
	s.mu.Unlock()

	for k, v := range resp.Header {
		if !isHopByHopHeader(k) {
			for _, val := range v {
				w.Header().Add(k, val)
			}
		}
	}
	w.Header().Set("X-AgentFixture-Recorded", "true")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	if s.upstream == nil {
		http.Error(w, "Passthrough requires upstream URL", http.StatusBadGateway)
		return
	}
	outURL := *s.upstream
	outURL.Path = r.URL.Path
	outURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequest(r.Method, outURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		if !isHopByHopHeader(k) {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

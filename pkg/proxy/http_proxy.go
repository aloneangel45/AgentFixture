package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/AgentFixture/pkg/cassette"
	"github.com/aloneangel45/AgentFixture/pkg/chaos"
	"github.com/aloneangel45/AgentFixture/pkg/matcher"
)

// MaxHTTPBodyBytes caps non-SSE HTTP request and response bodies at 64 MiB.
const MaxHTTPBodyBytes int64 = 64 << 20

var errBodyTooLarge = errors.New("http body exceeds 64 MiB limit")

// Mode determines proxy runtime behavior.
type Mode string

const (
	ModeReplay      Mode = "replay"
	ModeRecord      Mode = "record"
	ModePassthrough Mode = "passthrough"
)

// RecordMode controls hybrid offline replay vs upstream recording behavior.
type RecordMode string

const (
	// RecordModeNone enforces strict offline replay with zero outbound network calls.
	RecordModeNone RecordMode = "none"
	// RecordModeNewEpisodes replays matched interactions offline and forwards only unmatched
	// requests to UpstreamURL, appending newly recorded episodes to the cassette.
	RecordModeNewEpisodes RecordMode = "new_episodes"
	// RecordModeAll records all interactions against UpstreamURL.
	RecordModeAll RecordMode = "all"
)

// ParseRecordMode parses a CLI or configuration string into a validated RecordMode.
func ParseRecordMode(raw string) (RecordMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none":
		return RecordModeNone, nil
	case "new_episodes":
		return RecordModeNewEpisodes, nil
	case "all":
		return RecordModeAll, nil
	default:
		return "", fmt.Errorf("unsupported record mode %q (expected none, new_episodes, or all)", raw)
	}
}

// DriftRecord captures a non-exact (<1.0) semantic match observed during replay.
type DriftRecord struct {
	InteractionID string
	ToolOrPath    string
	Similarity    float64
	Reason        string
}

// Config configures the HTTP proxy/replay server.
type Config struct {
	Mode           Mode
	RecordMode     RecordMode
	UpstreamURL    string
	Cassette       *cassette.Cassette
	Matcher        matcher.Matcher
	SimulateDelay  bool
	ListenAddr     string
	MaxReplayDelay time.Duration
	SSETiming      cassette.SSETimingConfig
	Chaos          *chaos.Injector
}

// Server provides deterministic HTTP/SSE playback and recording interception.
type Server struct {
	cfg            Config
	listener       net.Listener
	httpServer     *http.Server
	upstreamClient *http.Client
	inFlightMu     sync.Mutex
	inFlightCond   *sync.Cond
	inFlight       int
	mu             sync.Mutex
	cassette       *cassette.Cassette
	matchedIDs     map[string]bool
	unmatched      []cassette.Request
	driftRecords   []DriftRecord
	newEpisodes    int
	upstream       *url.URL
}

// NewServer initializes a proxy Server with the given configuration.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeReplay
	}
	if cfg.RecordMode == "" {
		if cfg.Mode == ModeRecord {
			cfg.RecordMode = RecordModeAll
		} else {
			cfg.RecordMode = RecordModeNone
		}
	} else {
		parsedRM, err := ParseRecordMode(string(cfg.RecordMode))
		if err != nil {
			return nil, err
		}
		cfg.RecordMode = parsedRM
	}
	if cfg.RecordMode == RecordModeNewEpisodes && strings.TrimSpace(cfg.UpstreamURL) == "" {
		return nil, fmt.Errorf("record mode %q requires an upstream URL", RecordModeNewEpisodes)
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
	if strings.TrimSpace(cfg.UpstreamURL) != "" {
		upstreamParsed, err = url.Parse(strings.TrimSpace(cfg.UpstreamURL))
		if err != nil {
			return nil, fmt.Errorf("invalid upstream URL %s: %w", cfg.UpstreamURL, err)
		}
		if (upstreamParsed.Scheme != "http" && upstreamParsed.Scheme != "https") || upstreamParsed.Host == "" {
			return nil, fmt.Errorf("invalid upstream URL %q: must include http:// or https:// scheme and host", cfg.UpstreamURL)
		}
	}

	var cas *cassette.Cassette
	if cfg.Cassette != nil {
		cas = cfg.Cassette.Clone()
	} else {
		cas = cassette.NewCassette(fmt.Sprintf("cas_%d", time.Now().UnixNano()))
	}

	s := &Server{
		cfg:          cfg,
		cassette:     cas,
		matchedIDs:   make(map[string]bool),
		unmatched:    make([]cassette.Request, 0),
		driftRecords: make([]DriftRecord, 0),
		upstream:     upstreamParsed,
		upstreamClient: &http.Client{
			// No fixed Client.Timeout so long-running LLM SSE / thinking streams never abort mid-flight.
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}

	// Register handler directly (bypassing ServeMux path redirects so HTTP CONNECT and proxy URIs work cleanly).
	s.inFlightCond = sync.NewCond(&s.inFlightMu)
	s.httpServer = &http.Server{
		Handler:           http.HandlerFunc(s.handleHTTP),
		ReadHeaderTimeout: 15 * time.Second,
	}

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

// WaitIdle blocks until all active HTTP handler goroutines finish recording or replaying.
func (s *Server) WaitIdle() {
	s.inFlightMu.Lock()
	for s.inFlight > 0 {
		s.inFlightCond.Wait()
	}
	s.inFlightMu.Unlock()
}

// Close gracefully drains active handlers, shuts down the server, and releases network ports.
func (s *Server) Close() error {
	var err error
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.httpServer.Shutdown(ctx)
		if err != nil {
			_ = s.httpServer.Close()
		}
	}
	s.WaitIdle()
	return err
}

// Mode returns the configured proxy Mode.
func (s *Server) Mode() Mode {
	return s.cfg.Mode
}

// RecordMode returns the effective RecordMode (none, new_episodes, or all).
func (s *Server) RecordMode() RecordMode {
	return s.cfg.RecordMode
}

// NewEpisodesCount returns how many unmatched interactions were forwarded and recorded
// in RecordModeNewEpisodes (or RecordModeAll) during this server's lifetime.
func (s *Server) NewEpisodesCount() int {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newEpisodes
}

// ChaosStats returns a snapshot of deterministic faults injected by this server.
func (s *Server) ChaosStats() chaos.Stats {
	return s.cfg.Chaos.Stats()
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
	if s.Addr() == "" {
		return ""
	}
	return "http://" + s.Addr()
}

// GetCassette returns a thread-safe clone of the Cassette containing recorded or loaded interactions.
func (s *Server) GetCassette() *cassette.Cassette {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cassette.Clone()
}

// MatchedCount returns how many unique cassette interactions have been replayed.
func (s *Server) MatchedCount() int {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.matchedIDs)
}

// MatchedIDs returns a snapshot map of all cassette interaction IDs matched during replay.
func (s *Server) MatchedIDs() map[string]bool {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]bool, len(s.matchedIDs))
	for k, v := range s.matchedIDs {
		cp[k] = v
	}
	return cp
}

// DriftRecords returns any non-exact semantic matches recorded during fuzzy replay.
func (s *Server) DriftRecords() []DriftRecord {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]DriftRecord, len(s.driftRecords))
	copy(cp, s.driftRecords)
	return cp
}

// UnmatchedRequests returns any requests received during replay that failed to match.
func (s *Server) UnmatchedRequests() []cassette.Request {
	s.WaitIdle()
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]cassette.Request, len(s.unmatched))
	for i := range s.unmatched {
		copied[i] = s.unmatched[i].Clone()
	}
	return copied
}

func readBoundedBody(r io.Reader, maxBytes int64) ([]byte, error) {
	lr := io.LimitReader(r, maxBytes+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, errBodyTooLarge
	}
	return b, nil
}

// nextInteractionID scans existing interactions to allocate a collision-free ID and monotonic sequence number.
func nextInteractionID(interactions []cassette.Interaction, prefix string) (string, int) {
	if prefix == "" {
		prefix = "int_"
	}
	seen := make(map[string]bool, len(interactions))
	maxSeq := len(interactions)

	for _, inter := range interactions {
		seen[inter.ID] = true
		if inter.Sequence > maxSeq {
			maxSeq = inter.Sequence
		}
		for _, p := range []string{prefix, "int_", "mcp_"} {
			if strings.HasPrefix(inter.ID, p) {
				if n, err := strconv.Atoi(inter.ID[len(p):]); err == nil && n > maxSeq {
					maxSeq = n
				}
			}
		}
	}

	seq := maxSeq + 1
	for {
		candidate := fmt.Sprintf("%s%03d", prefix, seq)
		if !seen[candidate] {
			return candidate, seq
		}
		seq++
	}
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	s.inFlightMu.Lock()
	s.inFlight++
	s.inFlightMu.Unlock()
	defer func() {
		s.inFlightMu.Lock()
		s.inFlight--
		if s.inFlight == 0 {
			s.inFlightCond.Broadcast()
		}
		s.inFlightMu.Unlock()
	}()
	// Zero Outbound Network Guard: strictly forbid HTTPS CONNECT tunneling in replay mode (and unsupported tunnel proxy use).
	if r.Method == http.MethodConnect {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AgentFixture-Blocked", "connect-tunnel-forbidden")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":  "AgentFixture blocked HTTP CONNECT tunnel: zero outbound network connections permitted",
			"target": r.Host,
		})
		return
	}

	bodyBytes, err := readBoundedBody(r.Body, MaxHTTPBodyBytes)
	_ = r.Body.Close()
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "Request body exceeds maximum 64 MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	var jsonBody interface{}
	trimmedReq := bytes.TrimSpace(bodyBytes)
	if len(trimmedReq) > 0 && (strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") || trimmedReq[0] == '{' || trimmedReq[0] == '[') {
		if parsed, decErr := matcher.DecodeJSONUseNumber(trimmedReq); decErr == nil {
			jsonBody = parsed
		}
	}

	endpoint := r.URL.Path
	if endpoint == "" {
		endpoint = "/"
	}
	if r.URL.RawQuery != "" {
		endpoint = endpoint + "?" + r.URL.RawQuery
	}

	candidate := cassette.Request{
		Method:   r.Method,
		Endpoint: endpoint,
		Headers:  headers,
		Body:     jsonBody,
		RawBody:  string(bodyBytes),
	}
	candidate.Matcher.StrictHash = matcher.ComputeRequestHash(&candidate)

	switch s.cfg.Mode {
	case ModeReplay:
		if s.cfg.RecordMode == RecordModeAll {
			s.handleRecord(w, r, &candidate, bodyBytes, true)
		} else {
			s.handleReplay(w, r, &candidate, bodyBytes)
		}
	case ModeRecord:
		switch s.cfg.RecordMode {
		case RecordModeNone, RecordModeNewEpisodes:
			s.handleReplay(w, r, &candidate, bodyBytes)
		default:
			s.handleRecord(w, r, &candidate, bodyBytes, true)
		}
	case ModePassthrough:
		s.handlePassthrough(w, r, bodyBytes)
	default:
		http.Error(w, "Unsupported proxy mode", http.StatusInternalServerError)
	}
}

func (s *Server) selectMatchLocked(candidate *cassette.Request) (*cassette.Interaction, float64, string) {
	var bestInter *cassette.Interaction
	var bestScore float64
	var lastDiff string

	// Pass 1: Prefer unconsumed interactions in recorded sequence order for stateful multi-turn agents.
	for i := range s.cassette.Interactions {
		inter := &s.cassette.Interactions[i]
		if s.matchedIDs[inter.ID] {
			continue
		}
		res := s.cfg.Matcher.Match(&inter.Request, candidate)
		if res.Matched && res.Score > bestScore {
			bestScore = res.Score
			bestInter = inter
			if res.Score == 1.0 {
				return bestInter, bestScore, ""
			}
		}
		if res.Diff != "" {
			lastDiff = res.Diff
		}
	}
	if bestInter != nil {
		return bestInter, bestScore, lastDiff
	}

	// Pass 2: Fallback to already-matched interactions for idempotent retries.
	for i := range s.cassette.Interactions {
		inter := &s.cassette.Interactions[i]
		if !s.matchedIDs[inter.ID] {
			continue
		}
		res := s.cfg.Matcher.Match(&inter.Request, candidate)
		if res.Matched && res.Score > bestScore {
			bestScore = res.Score
			bestInter = inter
			if res.Score == 1.0 {
				return bestInter, bestScore, ""
			}
		}
		if res.Diff != "" {
			lastDiff = res.Diff
		}
	}
	return bestInter, bestScore, lastDiff
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request, candidate *cassette.Request, bodyBytes []byte) {
	var dropSSEAfter int
	if s.cfg.Chaos != nil && s.cfg.Chaos.Enabled() {
		dec := s.cfg.Chaos.Evaluate()
		if dec.Latency > 0 {
			if err := chaos.SleepContext(r.Context(), dec.Latency); err != nil {
				return
			}
		}
		if dec.Fault != chaos.FaultNone {
			chaos.WriteHTTPFault(w, dec.Fault)
			return
		}
		dropSSEAfter = dec.DropSSEAfter
	}

	redactedCandidate := candidate.Clone()
	cassette.RedactRequest(&redactedCandidate)
	redactedCandidate.Matcher.StrictHash = matcher.ComputeRequestHash(&redactedCandidate)

	s.mu.Lock()
	bestInter, bestScore, lastDiff := s.selectMatchLocked(&redactedCandidate)
	if bestInter == nil && redactedCandidate.Matcher.StrictHash != candidate.Matcher.StrictHash {
		// Fallback for programmatic in-memory test cassettes constructed without RedactRequest.
		bestInter, bestScore, lastDiff = s.selectMatchLocked(candidate)
	}

	if bestInter == nil {
		if s.cfg.RecordMode == RecordModeNewEpisodes {
			s.mu.Unlock()
			s.handleRecord(w, r, candidate, bodyBytes, true)
			return
		}
		s.unmatched = append(s.unmatched, redactedCandidate)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AgentFixture-Mismatch", "true")
		w.WriteHeader(http.StatusNotFound)

		resp := map[string]interface{}{
			"error":              "AgentFixture replay mismatch: no recorded interaction matched request",
			"candidate_method":   redactedCandidate.Method,
			"candidate_endpoint": redactedCandidate.Endpoint,
			"diff_explanation":   cassette.RedactString(lastDiff),
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	s.matchedIDs[bestInter.ID] = true
	if bestScore > 0 && bestScore < 1.0 {
		toolOrPath := bestInter.Request.ToolName
		if toolOrPath == "" {
			toolOrPath = bestInter.Request.Endpoint
		}
		reason := lastDiff
		if reason == "" {
			reason = fmt.Sprintf("matched via %s matcher below exact 1.0 score", s.cfg.Matcher.Name())
		}
		s.driftRecords = append(s.driftRecords, DriftRecord{
			InteractionID: bestInter.ID,
			ToolOrPath:    toolOrPath,
			Similarity:    bestScore,
			Reason:        reason,
		})
	}
	matchedSnapshot := bestInter.Clone()
	// Release lock BEFORE sleeping, writing headers, or streaming SSE chunks!
	s.mu.Unlock()

	if s.cfg.SimulateDelay && matchedSnapshot.Response.LatencyMs > 0 &&
		matchedSnapshot.Type != cassette.InteractionTypeSSE && len(matchedSnapshot.Response.SSEEvents) == 0 {
		sleepDur := time.Duration(matchedSnapshot.Response.LatencyMs) * time.Millisecond
		if sleepDur > s.cfg.MaxReplayDelay {
			sleepDur = s.cfg.MaxReplayDelay
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(sleepDur):
		}
	}

	for k, v := range matchedSnapshot.Response.Headers {
		if !isHopByHopOrEncodingHeader(k) {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("X-AgentFixture-Replay", "true")
	w.Header().Set("X-AgentFixture-Interaction-ID", matchedSnapshot.ID)

	status := matchedSnapshot.Response.StatusCode
	if status == 0 {
		status = http.StatusOK
	}

	isSSE := matchedSnapshot.Type == cassette.InteractionTypeSSE ||
		len(matchedSnapshot.Response.SSEEvents) > 0 ||
		strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream")

	if isSSE {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-cache")

		events := matchedSnapshot.Response.SSEEvents
		if len(events) == 0 && matchedSnapshot.Response.RawBody != "" {
			events, _ = cassette.DecodeSSE(strings.NewReader(matchedSnapshot.Response.RawBody))
		}
		flusher, _ := w.(http.Flusher)

		if dropSSEAfter > 0 && len(events) >= dropSSEAfter {
			w.Header().Set("X-AgentFixture-Chaos", "drop_sse")
			w.WriteHeader(status)
			if streamErr := cassette.ReplaySSEStream(r.Context(), w, flusher, events[:dropSSEAfter], s.cfg.SSETiming); streamErr != nil || r.Context().Err() != nil {
				// Client disconnected or context canceled before chaos drop point.
				return
			}
			s.cfg.Chaos.RecordSSEDrop()
			if hj, ok := w.(http.Hijacker); ok {
				if conn, bufrw, hErr := hj.Hijack(); hErr == nil {
					if bufrw != nil && bufrw.Writer != nil && bufrw.Writer.Buffered() > 0 {
						_ = bufrw.Flush()
					}
					_ = conn.Close()
					return
				}
			}
			// Non-hijackable ResponseWriter fallback: emit deterministic SSE error frame and close.
			errFrame := cassette.SSEEvent{
				Event: cassette.SSEEventError,
				Data:  fmt.Sprintf(`{"type":"error","error":{"type":"stream_interrupted","message":"SSE stream truncated after %d events (AgentFixture chaos injection)"}}`, dropSSEAfter),
			}
			_, _ = w.Write(cassette.EncodeSSEEvent(errFrame))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.WriteHeader(status)
		_ = cassette.ReplaySSEStream(r.Context(), w, flusher, events, s.cfg.SSETiming)
		return
	}

	w.WriteHeader(status)
	if matchedSnapshot.Response.RawBody != "" {
		_, _ = w.Write([]byte(matchedSnapshot.Response.RawBody))
	} else if matchedSnapshot.Response.ToolResult != "" {
		_, _ = w.Write([]byte(matchedSnapshot.Response.ToolResult))
	} else if matchedSnapshot.Response.Body != nil {
		_ = json.NewEncoder(w).Encode(matchedSnapshot.Response.Body)
	}
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request, candidate *cassette.Request, bodyBytes []byte, markMatched bool) {
	if s.upstream == nil {
		http.Error(w, "Proxy record mode requires an upstream target URL", http.StatusBadGateway)
		return
	}

	targetURL := joinUpstreamURL(s.upstream, r.URL)
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to construct upstream request: %v", err), http.StatusInternalServerError)
		return
	}
	outReq.Host = s.upstream.Host

	for k, vv := range r.Header {
		// Strip Accept-Encoding & Content-Length so Go's http.Transport transparently negotiates and decompresses gzip!
		if !isHopByHopOrEncodingHeader(k) && !strings.EqualFold(k, "Accept-Encoding") && !strings.EqualFold(k, "Host") {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}
	}

	start := time.Now()
	resp, err := s.upstreamClient.Do(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream request failed: %s", cassette.RedactString(err.Error())), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var bodyReader io.Reader = resp.Body
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		gzReader, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			http.Error(w, fmt.Sprintf("Failed to initialize upstream gzip reader: %v", gzErr), http.StatusBadGateway)
			return
		}
		defer gzReader.Close()
		bodyReader = gzReader
	}

	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 && !isHopByHopOrEncodingHeader(k) {
			respHeaders[k] = strings.Join(v, ", ")
		}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		for k, vv := range resp.Header {
			if !isHopByHopOrEncodingHeader(k) {
				for _, val := range vv {
					w.Header().Add(k, val)
				}
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-AgentFixture-Recorded", "true")
		w.WriteHeader(resp.StatusCode)

		flusher, _ := w.(http.Flusher)
		events, streamErr := cassette.RecordSSEStream(bodyReader, func(ev cassette.SSEEvent) error {
			if ctxErr := r.Context().Err(); ctxErr != nil {
				return ctxErr
			}
			encoded := cassette.EncodeSSEEvent(ev)
			if _, wErr := w.Write(encoded); wErr != nil {
				return wErr
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		if (streamErr != nil && streamErr != io.EOF) || r.Context().Err() != nil {
			// Do not record truncated or aborted mid-flight SSE streams into the cassette.
			return
		}
		duration := time.Since(start).Milliseconds()

		summary := cassette.ReconstructStreamSummary(events)
		meta := make(map[string]string)
		if summary.Provider != "" {
			meta["provider"] = summary.Provider
		}
		if summary.Model != "" {
			meta["model"] = summary.Model
		}
		if summary.StopReason != "" {
			meta["stop_reason"] = summary.StopReason
		}
		if summary.MessageID != "" {
			meta["message_id"] = summary.MessageID
		}

		storedReq := candidate.Clone()
		cassette.RedactRequest(&storedReq)
		storedReq.Matcher.StrictHash = matcher.ComputeRequestHash(&storedReq)
		normPrompt, reqModel := matcher.ExtractPromptAndModel(&storedReq)
		if normPrompt != "" {
			storedReq.Matcher.FuzzySignature.NormalizedPrompt = normPrompt
		}
		resolvedModel := summary.Model
		if resolvedModel == "" {
			resolvedModel = reqModel
		}
		if resolvedModel != "" {
			storedReq.Matcher.FuzzySignature.Model = resolvedModel
		}

		inter := cassette.Interaction{
			Type:    cassette.InteractionTypeSSE,
			Request: storedReq,
			Response: cassette.Response{
				StatusCode: resp.StatusCode,
				LatencyMs:  duration,
				Headers:    respHeaders,
				SSEEvents:  events,
				Usage:      summary.Usage,
			},
			Metadata: meta,
		}
		cassette.RedactInteraction(&inter)

		s.mu.Lock()
		id, seq := nextInteractionID(s.cassette.Interactions, "int_")
		inter.ID = id
		inter.Sequence = seq
		if s.cassette.Metadata.Model == "" && resolvedModel != "" {
			s.cassette.Metadata.Model = resolvedModel
		}
		s.cassette.Interactions = append(s.cassette.Interactions, inter)
		s.newEpisodes++
		if markMatched {
			s.matchedIDs[inter.ID] = true
		}
		s.mu.Unlock()
		return
	}

	respBody, err := readBoundedBody(bodyReader, MaxHTTPBodyBytes)
	duration := time.Since(start).Milliseconds()
	if err != nil || r.Context().Err() != nil {
		if r.Context().Err() == nil {
			http.Error(w, fmt.Sprintf("Failed to read upstream response body: %v", err), http.StatusBadGateway)
		}
		return
	}

	var respJSON interface{}
	var usage cassette.Usage
	var detectedModel string
	trimmedResp := bytes.TrimSpace(respBody)
	if len(trimmedResp) > 0 && (strings.Contains(contentType, "json") || trimmedResp[0] == '{' || trimmedResp[0] == '[') {
		if parsed, decErr := matcher.DecodeJSONUseNumber(trimmedResp); decErr == nil {
			respJSON = parsed
			usage = cassette.ExtractUsageFromBody(parsed)
			detectedModel = cassette.ExtractModelFromBody(parsed)
		}
	}

	storedReq := candidate.Clone()
	cassette.RedactRequest(&storedReq)
	storedReq.Matcher.StrictHash = matcher.ComputeRequestHash(&storedReq)
	normPrompt, reqModel := matcher.ExtractPromptAndModel(&storedReq)
	if normPrompt != "" {
		storedReq.Matcher.FuzzySignature.NormalizedPrompt = normPrompt
	}
	if detectedModel == "" {
		detectedModel = reqModel
	}
	if detectedModel != "" {
		storedReq.Matcher.FuzzySignature.Model = detectedModel
	}

	inter := cassette.Interaction{
		Type:    cassette.InteractionTypeHTTP,
		Request: storedReq,
		Response: cassette.Response{
			StatusCode: resp.StatusCode,
			LatencyMs:  duration,
			Headers:    respHeaders,
			Body:       respJSON,
			RawBody:    string(respBody),
			Usage:      usage,
		},
	}
	cassette.RedactInteraction(&inter)

	s.mu.Lock()
	id, seq := nextInteractionID(s.cassette.Interactions, "int_")
	inter.ID = id
	inter.Sequence = seq
	if s.cassette.Metadata.Model == "" && detectedModel != "" {
		s.cassette.Metadata.Model = detectedModel
	}
	s.cassette.Interactions = append(s.cassette.Interactions, inter)
	s.newEpisodes++
	if markMatched {
		s.matchedIDs[inter.ID] = true
	}
	s.mu.Unlock()

	for k, vv := range resp.Header {
		if !isHopByHopOrEncodingHeader(k) {
			for _, val := range vv {
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
	targetURL := joinUpstreamURL(s.upstream, r.URL)
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.Host = s.upstream.Host

	for k, vv := range r.Header {
		if !isHopByHopHeader(k) && !strings.EqualFold(k, "Host") {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}
	}

	resp, err := s.upstreamClient.Do(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if !isHopByHopHeader(k) {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		buf := make([]byte, 4096)
		for {
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				flusher.Flush()
			}
			if rErr != nil {
				break
			}
		}
		return
	}
	_, _ = io.Copy(w, io.LimitReader(resp.Body, MaxHTTPBodyBytes))
}

func joinUpstreamURL(upstream *url.URL, reqURL *url.URL) string {
	out := *upstream
	basePath := strings.TrimRight(upstream.Path, "/")
	reqPath := reqURL.Path
	if reqPath == "" {
		reqPath = "/"
	} else if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}

	if basePath == "" {
		out.Path = reqPath
	} else if reqPath == "/" {
		out.Path = basePath + "/"
	} else if reqPath == basePath || strings.HasPrefix(reqPath, basePath+"/") {
		out.Path = reqPath
	} else {
		out.Path = basePath + reqPath
	}
	out.RawPath = ""

	switch {
	case upstream.RawQuery != "" && reqURL.RawQuery != "":
		out.RawQuery = upstream.RawQuery + "&" + reqURL.RawQuery
	case reqURL.RawQuery != "":
		out.RawQuery = reqURL.RawQuery
	default:
		out.RawQuery = upstream.RawQuery
	}
	return out.String()
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

func isHopByHopOrEncodingHeader(name string) bool {
	if isHopByHopHeader(name) {
		return true
	}
	switch strings.ToLower(name) {
	case "content-encoding", "content-length":
		return true
	default:
		return false
	}
}

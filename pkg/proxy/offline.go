package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrOutboundNetworkBlocked is returned whenever an offline HTTP client attempts to dial a non-loopback address.
var ErrOutboundNetworkBlocked = errors.New("agentfixture: outbound network access blocked in offline replay mode")

// IsLoopbackHost reports whether host refers strictly to a local loopback interface (127.0.0.0/8, ::1, or localhost)
// without performing external DNS resolution.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if zoneIdx := strings.IndexByte(h, '%'); zoneIdx != -1 {
		h = h[:zoneIdx]
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// NewOfflineHTTPClient constructs an *http.Client guaranteed to make zero outbound network connections.
// Its custom DialContext blocks all TCP/UDP dials to non-loopback addresses (anything outside 127.0.0.1, ::1, localhost).
// When proxyURL is non-empty, plain HTTP requests to external hostnames are routed through the local replay proxy,
// while HTTPS CONNECT tunnels are rejected with 403 Forbidden.
func NewOfflineHTTPClient(proxyURL string) *http.Client {
	var parsedProxy *url.URL
	if strings.TrimSpace(proxyURL) != "" {
		if u, err := url.Parse(strings.TrimSpace(proxyURL)); err == nil && u.Host != "" {
			parsedProxy = u
		}
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if parsedProxy == nil {
				return nil, nil
			}
			// Requests already targeting loopback connect directly without proxy indirection.
			if strings.EqualFold(req.URL.Host, parsedProxy.Host) || IsLoopbackHost(req.URL.Hostname()) {
				return nil, nil
			}
			return parsedProxy, nil
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid address %q: %v", ErrOutboundNetworkBlocked, addr, err)
			}
			if !IsLoopbackHost(host) {
				return nil, fmt.Errorf("%w: attempted %s connection to %s", ErrOutboundNetworkBlocked, network, addr)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		ForceAttemptHTTP2:     false,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	return &http.Client{
		Transport: transport,
	}
}

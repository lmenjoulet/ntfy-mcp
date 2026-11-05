// MCP server exposing tools to send and read notifications on an ntfy server.
//
// Configuration via environment variables:
//
//	NTFY_URL      Default ntfy server URL (default: https://ntfy.sh)
//	NTFY_TOPIC    Default topic (default: agent)
//	NTFY_TOKEN    Default token (default: empty)
//
//	MCP_NTFY_TRANSPORT  Transport: "http" (default) or "stdio"
//	MCP_NTFY_ADDR MCP server listen address, http transport only (default: 127.0.0.1)
//	MCP_NTFY_PORT MCP server port, http transport only (default: 12120)
//	MCP_NTFY_CLIENT_TOKEN  When set, every MCP request must carry this token
//	                      in its "Authorization: Bearer" header (default: empty,
//	                      no client authentication)
//
//	MCP_NTFY_RATE_LIMIT_FAILURES  Upstream rejections (HTTP 401/403/429) per
//	                              target base URL tolerated within the window
//	                              before the target is blocked (default: 5)
//	MCP_NTFY_RATE_LIMIT_WINDOW    Sliding window for counting upstream
//	                              rejections, in seconds (default: 60)
//	MCP_NTFY_RATE_LIMIT_BLOCK     How long a target stays blocked, in seconds
//	                              (default: 300); 0 or a negative failure
//	                              limit disables the guard
//
//	MCP_NTFY_UNSAFE_LOGS  When set to "1", authentication tokens are logged
//	                      in cleartext (unsafe, debugging only; default: off)
//
//	MCP_NTFY_ALLOW_PRIVATE_UPSTREAM  When "1", upstream connections to
//	                      private/loopback/link-local IPs are allowed (for a
//	                      self-hosted ntfy on the local network; default: "0",
//	                      such IPs are refused at dial time, anti-SSRF)
//	MCP_NTFY_CLIENT_RATE_LIMIT  Max MCP requests per client IP within
//	                      MCP_NTFY_RATE_LIMIT_WINDOW seconds (default: 120;
//	                      0 disables; over the limit -> HTTP 429)
//	MCP_NTFY_AUTH_FAILURE_LIMIT  Max failed client authentications per IP
//	                      within the window before HTTP 429 (default: 10)
//	MCP_NTFY_MAX_CONCURRENT_UPSTREAM  Max concurrent upstream ntfy calls
//	                      (default: 32)
//
// An agent can override any of these variables in an MCP request.
// Configure them according to your security model.
// Example: Public MCP with known token, or private MCP.
// Usage: ntfy-mcp (http transport) or MCP_NTFY_TRANSPORT=stdio ntfy-mcp
// (stdio transport, the usual way for MCP clients to spawn a local server)
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var Version = "dev"

// Limits applied to protect the server itself (anti-DoS).
const (
	maxMCPBodyBytes  = 1 << 20  // cap on an MCP JSON-RPC request body
	maxMessageBytes  = 4096     // ntfy server limit for a notification body
	maxFetchBytes    = 4 << 20  // cap on total bytes read from an upstream stream
	defaultReadLimit = 100      // read_notifications limit when omitted
	maxReadLimit     = 1000     // hard cap on read_notifications limit
)

// httpClient is shared across all requests: its TCP connections are
// reused between calls (keep-alive), avoiding a full TLS handshake per
// notification. It is built by newHTTPClient once the config is loaded.
var httpClient *http.Client

// newHTTPClient builds the shared upstream HTTP client. Unless
// allowPrivateUpstream is set (MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1, meant
// for a self-hosted ntfy on a private network), every dialed address is
// checked: loopback, private, link-local (which includes the cloud
// metadata endpoint 169.254.169.254), CGNAT, multicast and unspecified
// IPs are refused. The check runs at dial time on the resolved
// addresses and the connection is pinned to a validated IP, so a DNS
// rebinding attack cannot bypass it (TLS ServerName still comes from
// the request URL, so certificate verification is unaffected).
func newHTTPClient(allowPrivateUpstream bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
			DialContext:         safeDialContext(dialer, allowPrivateUpstream),
		},
	}
}

func safeDialContext(dialer *net.Dialer, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil {
			if !allowPrivate && isBlockedUpstreamIP(ip) {
				return nil, &toolInputError{fmt.Sprintf("upstream address %s is not allowed (private, loopback, link-local or metadata IP)", ip)}
			}
			return dialer.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("upstream host %q resolves to no address", host)
		}
		for _, ip := range ips {
			if !allowPrivate && isBlockedUpstreamIP(ip.IP) {
				return nil, &toolInputError{fmt.Sprintf("upstream host %q resolves to a disallowed address (%s)", host, ip.IP)}
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

func isBlockedUpstreamIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true // CGNAT 100.64.0.0/10
	}
	return false
}

// toolInputError marks an error whose message is safe to return to the
// MCP client because it only describes the client's own input (bad
// base_url, bad topic, blocked target, ...). Any other error is logged
// server-side with full detail and replaced by a generic message, so
// internal details never leak to MCP clients.
type toolInputError struct{ msg string }

func (e *toolInputError) Error() string { return e.msg }

// config holds the configuration loaded once at startup.
type config struct {
	BaseURL              string // Base URL, already normalized (no trailing slash)
	Topic                string
	Token                string
	Transport            string // "http" or "stdio"
	MCPAddr              string // http transport only
	MCPPort              int    // http transport only
	ClientToken          string // Shared secret MCP clients must present; empty = no client auth
	UnsafeLogs           string
	AllowPrivateUpstream bool   // MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1: allow dialing private/loopback IPs upstream
	Guard                *upstreamGuard
	ClientLimiter        *rateLimiter // per-IP rate limit on MCP requests; nil = disabled
	AuthFailLimiter      *rateLimiter // per-IP throttle on failed client auth; nil = disabled
	upstreamSem          chan struct{} // bounds concurrent upstream calls
}

// upstreamGuard protects third-party ntfy servers from being flooded
// through this MCP server used as a proxy: when a target base URL
// accumulates too many rejection responses (HTTP 401/403/429) within a
// sliding window, that target is temporarily blocked and requests to it
// are refused locally, without any upstream call.
//
// The guard is keyed by target base URL only (not by request token), so a
// client cannot evade the block by rotating tokens against the same
// target.
type upstreamGuard struct {
	mu           sync.Mutex
	maxFailures  int
	window       time.Duration
	blockFor     time.Duration
	maxEntries   int                          // cap on map sizes: targets are client-supplied, the maps must not grow without bound
	rejections   map[string][]time.Time // target -> timestamps of recent upstream rejections
	blockedUntil map[string]time.Time   // target -> end of the current block
}

func newUpstreamGuard(maxFailures, windowSec, blockSec int) *upstreamGuard {
	if maxFailures <= 0 {
		return nil // disabled
	}
	return &upstreamGuard{
		maxFailures:  maxFailures,
		window:       time.Duration(windowSec) * time.Second,
		blockFor:     time.Duration(blockSec) * time.Second,
		maxEntries:   10000,
		rejections:   map[string][]time.Time{},
		blockedUntil: map[string]time.Time{},
	}
}

// evictIfNeeded drops the oldest entries once a map exceeds maxEntries,
// so a client spraying unique base_url values cannot grow the guard's
// maps without bound.
func (g *upstreamGuard) evictIfNeeded() {
	if len(g.rejections) > g.maxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, v := range g.rejections {
			if oldestKey == "" || (len(v) > 0 && v[0].Before(oldest)) {
				oldestKey, oldest = k, v[0]
			}
		}
		delete(g.rejections, oldestKey)
	}
	if len(g.blockedUntil) > g.maxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, until := range g.blockedUntil {
			if oldestKey == "" || until.Before(oldest) {
				oldestKey, oldest = k, until
			}
		}
		delete(g.blockedUntil, oldestKey)
	}
}

// allow returns how long the target is still blocked; 0 means the request
// may be forwarded upstream.
func (g *upstreamGuard) allow(target string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, blocked := g.blockedUntil[target]
	if !blocked {
		return 0
	}
	if remaining := time.Until(until); remaining > 0 {
		return remaining
	}
	delete(g.blockedUntil, target)
	return 0
}

// noteRejection records an upstream response status for the target and
// blocks the target once maxFailures rejections (401/403/429) have been
// seen within the sliding window. Other statuses are ignored.
func (g *upstreamGuard) noteRejection(target string, status int) {
	if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-g.window)
	kept := g.rejections[target][:0]
	for _, t := range g.rejections[target] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.rejections[target] = append(kept, now)
	if len(g.rejections[target]) >= g.maxFailures {
		g.blockedUntil[target] = now.Add(g.blockFor)
		delete(g.rejections, target)
		log.Printf("upstream target %q BLOCKED for %s after %d rejections (HTTP %d) within %s",
			target, g.blockFor, g.maxFailures, status, g.window)
	}
	g.evictIfNeeded()
}

// checkUpstreamAllowed refuses the request locally when the target is
// currently blocked by the anti-DoS guard.
func (c config) checkUpstreamAllowed(baseURL string) error {
	if c.Guard == nil {
		return nil
	}
	if remaining := c.Guard.allow(baseURL); remaining > 0 {
		log.Printf("BLOCKED request to %q: target temporarily blocked after repeated upstream rejections (retry in %s)",
			baseURL, remaining.Round(time.Second))
		return &toolInputError{fmt.Sprintf("target %s is temporarily blocked after repeated rejected requests (upstream 401/403/429); retry in %s",
			baseURL, remaining.Round(time.Second))}
	}
	return nil
}

// noteUpstreamStatus feeds the upstream response status to the anti-DoS
// guard (no-op when the guard is disabled or the status is not a
// rejection).
func (c config) noteUpstreamStatus(baseURL string, status int) {
	if c.Guard == nil {
		return
	}
	c.Guard.noteRejection(baseURL, status)
}

func loadConfig() (config, error) {
	var cfg config

	mcpport, err := strconv.Atoi(getenv("MCP_NTFY_PORT", "12120"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_PORT environment variable: %w", err)
	}

	maxFail, err := strconv.Atoi(getenv("MCP_NTFY_RATE_LIMIT_FAILURES", "5"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_RATE_LIMIT_FAILURES environment variable: %w", err)
	}
	windowSec, err := strconv.Atoi(getenv("MCP_NTFY_RATE_LIMIT_WINDOW", "60"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_RATE_LIMIT_WINDOW environment variable: %w", err)
	}
	blockSec, err := strconv.Atoi(getenv("MCP_NTFY_RATE_LIMIT_BLOCK", "300"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_RATE_LIMIT_BLOCK environment variable: %w", err)
	}
	clientRateLimit, err := strconv.Atoi(getenv("MCP_NTFY_CLIENT_RATE_LIMIT", "120"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_CLIENT_RATE_LIMIT environment variable: %w", err)
	}
	authFailLimit, err := strconv.Atoi(getenv("MCP_NTFY_AUTH_FAILURE_LIMIT", "10"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_AUTH_FAILURE_LIMIT environment variable: %w", err)
	}
	maxConcurrent, err := strconv.Atoi(getenv("MCP_NTFY_MAX_CONCURRENT_UPSTREAM", "32"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_MAX_CONCURRENT_UPSTREAM environment variable: %w", err)
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 32
	}
	allowPrivate := isTruthy(getenv("MCP_NTFY_ALLOW_PRIVATE_UPSTREAM", "0"))

	transport := strings.ToLower(getenv("MCP_NTFY_TRANSPORT", "http"))
	if transport != "http" && transport != "stdio" {
		return cfg, fmt.Errorf("error getting MCP_NTFY_TRANSPORT environment variable: %q is not a valid transport (want \"http\" or \"stdio\")", transport)
	}

	cfg = config{
		BaseURL:              strings.TrimRight(getenv("NTFY_URL", "https://ntfy.sh"), "/"),
		Topic:                getenv("NTFY_TOPIC", "agent"),
		Token:                getenv("NTFY_TOKEN", ""),
		Transport:            transport,
		MCPAddr:              getenv("MCP_NTFY_ADDR", "127.0.0.1"),
		MCPPort:              mcpport,
		ClientToken:          getenv("MCP_NTFY_CLIENT_TOKEN", ""),
		UnsafeLogs:           getenv("MCP_NTFY_UNSAFE_LOGS", "0"),
		AllowPrivateUpstream: allowPrivate,
		Guard:                newUpstreamGuard(maxFail, windowSec, blockSec),
		ClientLimiter:        newRateLimiter(clientRateLimit, windowSec, 10000),
		AuthFailLimiter:      newRateLimiter(authFailLimit, windowSec, 10000),
		upstreamSem:          make(chan struct{}, maxConcurrent),
	}

	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return cfg, fmt.Errorf("invalid NTFY_URL: %w", err)
	}

	if transport == "stdio" && cfg.ClientToken != "" {
		log.Print("WARNING: MCP_NTFY_CLIENT_TOKEN is set but has no effect in stdio mode (no HTTP headers to check); ignoring it")
	}

	if transport == "http" && !isLoopbackAddr(cfg.MCPAddr) {
		if cfg.ClientToken == "" {
			log.Print("WARNING: listening on a non-loopback address without MCP_NTFY_CLIENT_TOKEN: any client can call the tools; expose the server only behind an authenticated TLS reverse proxy")
		}
		if cfg.UnsafeLogs == "1" {
			log.Print("WARNING: MCP_NTFY_UNSAFE_LOGS=1 on a non-loopback listen address: tokens will appear in cleartext in the logs of an exposed host")
		}
	}

	if !allowPrivate {
		log.Print("upstream SSRF guard: private/loopback/link-local/metadata IPs are refused at dial time (set MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1 only for a trusted internal ntfy server)")
	}
	log.Printf("client rate limit: %d MCP requests per %ds per client IP; %d failed authentications per %ds per IP before HTTP 429",
		clientRateLimit, windowSec, authFailLimit, windowSec)

	if cfg.Guard != nil {
		log.Printf("upstream anti-DoS guard enabled: %d rejections (401/403/429) within %ds block a target for %ds",
			maxFail, windowSec, blockSec)
	}

	if cfg.ClientToken != "" {
		log.Print("MCP client authentication enabled: requests must carry the MCP_NTFY_CLIENT_TOKEN as a Bearer token")
	}
	if cfg.UnsafeLogs == "1" {
		log.Print("WARNING: MCP_NTFY_UNSAFE_LOGS=1, tokens will appear in cleartext in the logs")
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isTruthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
}

// topicRe restricts topic names to the charset ntfy accepts, so a topic
// can never inject path segments ("../"), query ("?") or fragment ("#")
// into the upstream URL built by string concatenation.
var topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateBaseURL enforces the anti-SSRF first line: only absolute
// http/https URLs without embedded credentials are accepted as upstream
// targets.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return &toolInputError{"base_url must be a valid absolute http(s) URL without embedded credentials"}
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return &toolInputError{"base_url must use the http or https scheme"}
	}
	return nil
}

// isLoopbackAddr reports whether the configured listen address is
// loopback (IP or resolvable hostname), used to warn when the server is
// exposed beyond loopback without protection.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// rateLimiter is a per-key sliding-window limiter. Keys are client IPs;
// the map is capped so a flood of distinct keys cannot exhaust memory.
type rateLimiter struct {
	mu         sync.Mutex
	max        int
	window     time.Duration
	maxEntries int
	hits       map[string][]time.Time
}

func newRateLimiter(max, windowSec, maxEntries int) *rateLimiter {
	if max <= 0 {
		return nil // disabled
	}
	return &rateLimiter{
		max:        max,
		window:     time.Duration(windowSec) * time.Second,
		maxEntries: maxEntries,
		hits:       map[string][]time.Time{},
	}
}

// allow records a hit for key and reports whether it is within the
// limit (true) or over it (false).
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > l.maxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, v := range l.hits {
			if oldestKey == "" || (len(v) > 0 && v[0].Before(oldest)) {
				oldestKey, oldest = k, v[0]
			}
		}
		delete(l.hits, oldestKey)
	}
	return len(l.hits[key]) <= l.max
}

// clientIP extracts the IP of the direct peer. X-Forwarded-For is
// deliberately not trusted: a client could spoof it to dodge the
// per-IP limits.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withRateLimit caps the number of MCP requests per client IP within
// the sliding window (HTTP 429 when over) and caps the request body
// size, so neither slow nor huge requests can exhaust the server.
func withRateLimit(limiter *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(clientIP(r)) {
			log.Printf("RATE LIMITED request from %s to %s %s: too many requests", r.RemoteAddr, r.Method, r.URL.Path)
			http.Error(w, "too many requests, retry later", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether two raw base URLs point to the exact same
// destination: identical scheme and host (case-insensitive, port included)
// and identical path, with no userinfo component. Anything that fails to
// parse, or that carries a userinfo (e.g. "https://host@evil/"), is
// considered a different origin.
func sameOrigin(rawA, rawB string) bool {
	a, err := url.Parse(strings.TrimRight(rawA, "/"))
	if err != nil || a.User != nil || a.Scheme == "" || a.Host == "" {
		return false
	}
	b, err := url.Parse(strings.TrimRight(rawB, "/"))
	if err != nil || b.User != nil || b.Scheme == "" || b.Host == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Host, b.Host) &&
		a.Path == b.Path
}

// withClientAuth wraps the MCP endpoint with a shared-secret check, so
// that unauthenticated users cannot reach the tools (and spam the ntfy
// topic) when the server is exposed beyond loopback. When secret is empty
// the handler is returned unchanged: no client authentication.
//
// Clients must send "Authorization: Bearer <secret>". Both sides are
// hashed (SHA-256) before a constant-time comparison, so neither the
// secret's length nor its content leaks through timing. Repeated failed
// attempts from one IP are throttled (HTTP 429) to slow online brute
// force. The presented token is never logged.
func withClientAuth(secret string, failLimiter *rateLimiter, next http.Handler) http.Handler {
	if secret == "" {
		return next
	}
	secretHash := sha256.Sum256([]byte(secret))
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), prefix)
		presentedHash := sha256.Sum256([]byte(presented))
		if presented == r.Header.Get("Authorization") ||
			subtle.ConstantTimeCompare(presentedHash[:], secretHash[:]) != 1 {
			if !failLimiter.allow(clientIP(r)) {
				log.Printf("THROTTLED MCP request from %s to %s %s: too many failed authentication attempts", r.RemoteAddr, r.Method, r.URL.Path)
				http.Error(w, "too many failed authentication attempts, retry later", http.StatusTooManyRequests)
				return
			}
			log.Printf("REJECTED MCP request from %s to %s %s: missing or invalid client token", r.RemoteAddr, r.Method, r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="ntfy-mcp"`)
			http.Error(w, "unauthorized: a valid Bearer token is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notificationArgs describes the parameters for the send_notification tool.
type notificationArgs struct {
	BaseURL  string `json:"base_url,omitempty"`
	Token    string `json:"token,omitempty"`
	Message  string `json:"message"`
	Title    string `json:"title,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Tags     string `json:"tags,omitempty"` // Comma-separated (e.g., "warning,phone")
}

func parseArgs[T any](request mcp.CallToolRequest) (T, error) {
	var args T
	raw, err := json.Marshal(request.GetArguments())
	if err != nil {
		return args, fmt.Errorf("error reading arguments: %w", err)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("error decoding arguments: %w", err)
	}
	return args, nil
}

func (c config) publish(ctx context.Context, args notificationArgs) error {

	// Anti-exfiltration guard (shared with fetch): an agent-chosen
	// base_url is only accepted when the request carries its own token.
	// Otherwise the server's token could be sent to an arbitrary
	// destination. Origins are compared as parsed URLs (scheme + host +
	// path), never as raw strings, so look-alike spellings cannot slip
	// through.
	baseURL, token, topic, err := c.resolveTarget(args.BaseURL, args.Token, args.Topic)
	if err != nil {
		return err
	}
	if err := c.checkUpstreamAllowed(baseURL); err != nil {
		return err
	}
	if len(args.Message) > maxMessageBytes {
		return &toolInputError{fmt.Sprintf("message exceeds the %d-byte limit of the ntfy server", maxMessageBytes)}
	}
	if err := c.acquireUpstreamSlot(ctx); err != nil {
		return err
	}
	defer c.releaseUpstreamSlot()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/"+topic, strings.NewReader(args.Message))
	if err != nil {
		log.Printf("ERROR building request to %q: %q", baseURL+"/"+topic, err.Error())
		return fmt.Errorf("error building request: %w", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// The token is never logged, unless MCP_NTFY_UNSAFE_LOGS=1 was set
	// at startup (debugging only: logs are not a secret-safe place).
	logToken := "provided=%t"
	tokenArgs := any(token != "")
	if c.UnsafeLogs == "1" {
		logToken = "%q"
		tokenArgs = token
	}
	log.Printf("publishing: POST %q/%q (title=%q, priority=%d, tags=%q, token "+logToken+")",
		baseURL, topic, args.Title, args.Priority, args.Tags, tokenArgs)

	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	req.Header.Set("Title", args.Title)

	if args.Priority != 0 {
		req.Header.Set("Priority", strconv.Itoa(args.Priority))
	}
	if args.Tags != "" {
		req.Header.Set("Tags", args.Tags)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("ERROR sending to ntfy: %q", err.Error())
		var ie *toolInputError
		if errors.As(err, &ie) {
			return ie // e.g. dial-time refusal of a private/loopback target
		}
		return fmt.Errorf("error sending to ntfy: %w", err)
	}
	log.Printf("ntfy response: HTTP status code %d", resp.StatusCode)
	c.noteUpstreamStatus(baseURL, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The upstream body is logged server-side only: it is never
		// returned to the MCP client (it could carry internal data when
		// the target is not the expected ntfy server).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("ERROR ntfy response body: %q", strings.TrimSpace(string(body)))
		return fmt.Errorf("ntfy responded with status code %d", resp.StatusCode)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) //nolint:errcheck
	return nil
}

// acquireUpstreamSlot bounds the number of concurrent upstream calls so
// a burst of tool calls cannot exhaust file descriptors or memory.
func (c config) acquireUpstreamSlot(ctx context.Context) error {
	select {
	case c.upstreamSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("canceled while waiting for an upstream slot: %w", ctx.Err())
	}
}

func (c config) releaseUpstreamSlot() {
	<-c.upstreamSem
}

// readArgs describes the parameters for the read_notifications tool.
type readArgs struct {
	BaseURL    string `json:"base_url,omitempty"`
	Token      string `json:"token,omitempty"`
	Topic      string `json:"topic,omitempty"`
	Since      string `json:"since,omitempty"` // "all", a unix timestamp, or a notification ID
	Limit      int    `json:"limit,omitempty"` // Max number of notifications to return (client-side cap)
	CachedOnly bool   `json:"cached_only,omitempty"`
}

// ntfyNotification mirrors the JSON objects returned by the ntfy server
// when reading a topic (one JSON object per line).
type ntfyNotification struct {
	ID       string   `json:"id"`
	Time     int64    `json:"time"`
	Expires  int64    `json:"expires,omitempty"`
	Event    string   `json:"event,omitempty"`
	Topic    string   `json:"topic,omitempty"`
	Message  string   `json:"message,omitempty"`
	Title    string   `json:"title,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
	Icon     string   `json:"icon,omitempty"`
}

// resolveTarget applies the server-side defaults and enforces the same
// anti-exfiltration guard as publish: an agent-chosen base_url is only
// accepted when the request carries its own token.
func (c config) resolveTarget(baseURL, token, topic string) (string, string, string, error) {
	if baseURL == "" {
		baseURL = c.BaseURL
	}
	if baseURL != c.BaseURL && !sameOrigin(baseURL, c.BaseURL) && token == "" {
		log.Printf("REJECTED custom base_url %q without request token: server token must not leave to an agent-chosen URL", baseURL)
		return "", "", "", &toolInputError{"custom base_url requires a token in the request"}
	}
	// Anti-SSRF first line: only absolute http(s) URLs without embedded
	// credentials may be used as upstream targets.
	if err := validateBaseURL(baseURL); err != nil {
		log.Printf("REJECTED invalid base_url %q: %v", baseURL, err)
		return "", "", "", err
	}
	if token == "" {
		token = c.Token
	}
	if topic == "" {
		topic = c.Topic
	}
	if !topicRe.MatchString(topic) {
		log.Printf("REJECTED invalid topic %q", topic)
		return "", "", "", &toolInputError{"invalid topic: only letters, digits, '-' and '_' are allowed (max 64 characters)"}
	}
	return baseURL, token, topic, nil
}

// fetch retrieves cached notifications from a topic. It uses poll=1 so the
// server answers immediately with the messages it has in cache and closes
// the connection, instead of streaming new ones.
func (c config) fetch(ctx context.Context, args readArgs) ([]ntfyNotification, error) {
	baseURL, token, topic, err := c.resolveTarget(args.BaseURL, args.Token, args.Topic)
	if err != nil {
		return nil, err
	}
	if err := c.checkUpstreamAllowed(baseURL); err != nil {
		return nil, err
	}
	if err := c.acquireUpstreamSlot(ctx); err != nil {
		return nil, err
	}
	defer c.releaseUpstreamSlot()

	// Bound the number of notifications read: a default when omitted,
	// a hard cap always, plus a cap on the total bytes read, so a
	// malicious upstream cannot exhaust memory with a huge stream.
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}

	q := url.Values{}
	q.Set("poll", "1")
	if args.Since != "" {
		q.Set("since", args.Since)
	}
	if args.CachedOnly {
		q.Set("cached", "only")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/"+topic+"?"+q.Encode(), nil)
	if err != nil {
		log.Printf("ERROR building request to %q: %q", baseURL+"/"+topic, err.Error())
		return nil, fmt.Errorf("error building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Same logging policy as publish: the token is redacted unless
	// MCP_NTFY_UNSAFE_LOGS=1 was set at startup.
	logToken := "provided=%t"
	tokenArgs := any(token != "")
	if c.UnsafeLogs == "1" {
		logToken = "%q"
		tokenArgs = token
	}
	log.Printf("reading: GET %q/%q (since=%q, cached_only=%t, limit=%d, token "+logToken+")",
		baseURL, topic, args.Since, args.CachedOnly, limit, tokenArgs)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("ERROR reading from ntfy: %q", err.Error())
		var ie *toolInputError
		if errors.As(err, &ie) {
			return nil, ie // e.g. dial-time refusal of a private/loopback target
		}
		return nil, fmt.Errorf("error reading from ntfy: %w", err)
	}
	log.Printf("ntfy response: HTTP status code %d", resp.StatusCode)
	c.noteUpstreamStatus(baseURL, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Upstream body stays server-side (see publish).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("ERROR ntfy response body: %q", strings.TrimSpace(string(body)))
		return nil, fmt.Errorf("ntfy responded with status code %d", resp.StatusCode)
	}

	// ntfy streams one JSON object per line; parse them as they arrive
	// and stop early once the limit is reached or the byte cap is hit.
	var notifs []ntfyNotification
	var total int64
	scanner := bufio.NewScanner(&countingReader{r: resp.Body, n: &total})
	scanner.Buffer(make([]byte, 0, 64*1024), 256<<10)
	for scanner.Scan() {
		if total > maxFetchBytes {
			log.Printf("WARNING stopping ntfy stream after %d bytes (cap %d)", total, maxFetchBytes)
			break
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var n ntfyNotification
		if err := json.Unmarshal(line, &n); err != nil {
			log.Printf("WARNING skipping undecodable ntfy line: %q", err.Error())
			continue
		}
		notifs = append(notifs, n)
		if len(notifs) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("ERROR reading ntfy stream: %q", err.Error())
		return nil, fmt.Errorf("error reading ntfy stream: %w", err)
	}
	return notifs, nil
}

// countingReader tracks how many bytes were read from an upstream stream.
type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// buildNotificationTool declares the MCP tool and its schema.
func buildNotificationTool() mcp.Tool {
	return mcp.NewTool("send_notification",
		mcp.WithDescription(
			"Send a notification via an ntfy server. "+
				"Useful for reminders and scheduled task results."),
		mcp.WithString("base_url",
			mcp.Description("Base URL of the ntfy server (default: https://ntfy.sh)"),
		),
		mcp.WithString("token",
			mcp.Description("Token to authenticate with the ntfy server. Required when sending to a custom base_url; when omitted, the server-side token is used, only towards its configured URL."),
		),
		mcp.WithString("message",
			mcp.Required(),
			mcp.Description("Body of the notification."),
		),
		mcp.WithString("title",
			mcp.Description("Title of the notification."),
		),
		mcp.WithString("topic",
			mcp.Description("Destination topic (defaults to the server-side configured topic)."),
		),
		mcp.WithNumber("priority",
			mcp.Description("Priority from 1 (lowest) to 5 (highest)."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated tags, e.g., \"warning,phone\"."),
		),
	)
}

// notificationHandler applies the arguments and publishes the notification.
func notificationHandler(cfg config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs[notificationArgs](request)
		if err != nil {
			log.Printf("ERROR: %v", err)
			return nil, err
		}

		// Log the call with the token redacted by default: tokens must
		// never reach the logs. Setting MCP_NTFY_UNSAFE_LOGS=1 at startup
		// opts in to cleartext token logging, for debugging only. Client
		// fields are logged with %q so they cannot forge log lines.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("send_notification called with: base_url=%q topic=%q title=%q priority=%d tags=%q message_len=%d token=%q",
			logged.BaseURL, logged.Topic, logged.Title, logged.Priority, logged.Tags, len(args.Message), logged.Token)

		if strings.TrimSpace(args.Message) == "" {
			log.Print("ERROR: message parameter is empty")
			return nil, fmt.Errorf("message parameter cannot be empty")
		}

		if err := cfg.publish(ctx, args); err != nil {
			log.Printf("ERROR publishing failed: %v", err)
			var ie *toolInputError
			if errors.As(err, &ie) {
				return mcp.NewToolResultError(ie.Error()), nil
			}
			return mcp.NewToolResultError("failed to send the notification to the upstream ntfy server"), nil
		}
		log.Printf("notification published successfully")
		return mcp.NewToolResultText("Notification sent successfully."), nil
	}
}

// buildReadTool declares the read_notifications MCP tool and its schema.
func buildReadTool() mcp.Tool {
	return mcp.NewTool("read_notifications",
		mcp.WithDescription(
			"Read cached notifications from an ntfy topic. "+
				"Returns immediately with the messages the server has in cache; it does not wait for new ones."),
		mcp.WithString("base_url",
			mcp.Description("Base URL of the ntfy server (default: https://ntfy.sh)"),
		),
		mcp.WithString("token",
			mcp.Description("Token to authenticate with the ntfy server. Required when reading from a custom base_url; when omitted, the server-side token is used, only towards its configured URL."),
		),
		mcp.WithString("topic",
			mcp.Description("Topic to read from (defaults to the server-side configured topic)."),
		),
		mcp.WithString("since",
			mcp.Description("Only return notifications newer than this: \"all\", a unix timestamp, or a notification ID."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of notifications to return (most recent are kept)."),
		),
		mcp.WithBoolean("cached_only",
			mcp.Description("Only read messages already in the server cache, never wait for new ones (default: true behavior via poll)."),
		),
	)
}

// readHandler applies the arguments and fetches the notifications.
func readHandler(cfg config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs[readArgs](request)
		if err != nil {
			log.Printf("ERROR: %v", err)
			return nil, err
		}

		// Same redaction policy as send_notification, and %q on client
		// fields to prevent log forging.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("read_notifications called with: base_url=%q topic=%q since=%q limit=%d cached_only=%t token=%q",
			logged.BaseURL, logged.Topic, logged.Since, logged.Limit, logged.CachedOnly, logged.Token)

		notifs, err := cfg.fetch(ctx, args)
		if err != nil {
			log.Printf("ERROR reading failed: %v", err)
			var ie *toolInputError
			if errors.As(err, &ie) {
				return mcp.NewToolResultError(ie.Error()), nil
			}
			return mcp.NewToolResultError("failed to read notifications from the upstream ntfy server"), nil
		}
		if len(notifs) == 0 {
			log.Printf("no notifications found")
			return mcp.NewToolResultText("No notifications found."), nil
		}
		out, err := json.MarshalIndent(notifs, "", "  ")
		if err != nil {
			log.Printf("ERROR encoding notifications: %v", err)
			return nil, fmt.Errorf("error encoding notifications: %w", err)
		}
		log.Printf("returning %d notification(s)", len(notifs))
		return mcp.NewToolResultText(string(out)), nil
	}
}

func main() {
	log.SetPrefix("[ntfy-mcp] ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	cfg, err := loadConfig()

	if err != nil {
		log.Fatalf("%v", err)
	}

	httpClient = newHTTPClient(cfg.AllowPrivateUpstream)

	srv := server.NewMCPServer(
		"ntfy-mcp",
		Version,
		server.WithToolCapabilities(false),
	)

	srv.AddTool(buildNotificationTool(), notificationHandler(cfg))
	srv.AddTool(buildReadTool(), readHandler(cfg))

	if cfg.Transport == "stdio" {
		// Stdio transport: newline-delimited JSON-RPC on stdin/stdout,
		// the usual way MCP clients (Claude Desktop, etc.) spawn a local
		// server. Logs stay on stderr so they never pollute the protocol
		// channel on stdout.
		log.Printf("server v.%s serving MCP over stdio", Version)
		if err := server.ServeStdio(srv); err != nil {
			log.Fatalf("fatal error: %v", err)
		}
		return
	}

	transport := server.NewStreamableHTTPServer(srv)

	// Mount the MCP endpoint ourselves so the middlewares can wrap it:
	// withRateLimit (per-IP request cap + body size cap) runs first,
	// then withClientAuth (anti-spam, returns the handler unchanged when
	// MCP_NTFY_CLIENT_TOKEN is not set).
	mux := http.NewServeMux()
	mux.Handle("/mcp", transport)

	addr := fmt.Sprintf("%s:%d", cfg.MCPAddr, cfg.MCPPort)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: withRateLimit(cfg.ClientLimiter, withClientAuth(cfg.ClientToken, cfg.AuthFailLimiter, mux)),
		// Timeouts so slow or stalled clients cannot hold connections
		// (and goroutines) forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("server v.%s listening on %s (path /mcp)", Version, addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

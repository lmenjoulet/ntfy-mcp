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
//	                      such IPs are refused at dial time)
//	MCP_NTFY_CLIENT_RATE_LIMIT  Max MCP requests per client IP within
//	                      MCP_NTFY_RATE_LIMIT_WINDOW seconds (default: 120;
//	                      0 disables; over the limit -> HTTP 429)
//	MCP_NTFY_AUTH_FAILURE_LIMIT  Max failed client authentications per IP
//	                      within the window before HTTP 429 (default: 10)
//	MCP_NTFY_MAX_CONCURRENT_UPSTREAM  Max concurrent upstream ntfy calls
//	                      (default: 32)
//	MCP_NTFY_TRUSTED_PROXIES  Comma-separated IPs/CIDRs of reverse proxies
//	                      whose "X-Forwarded-For" header is trusted for the
//	                      per-client rate limits (default: empty, only the
//	                      direct peer IP is used). Set it only when a proxy
//	                      you control appends the real client IP.
//	MCP_NTFY_ALLOWED_HOSTS  Comma-separated public Host values (e.g. the
//	                      domain name the reverse proxy serves) accepted on
//	                      the MCP endpoint when listening on loopback: matching
//	                      requests get their Host header rewritten to the
//	                      loopback listen address so the transport's
//	                      DNS-rebinding guard accepts them (default: empty).
//	                      Prefer rewriting Host in the proxy itself
//	                      ("proxy_set_header Host 127.0.0.1;") when you
//	                      control it.
//
// In an MCP request, base_url, token and topic fall back to the configured
// values above.
// Usage: ntfy-mcp (http transport) or MCP_NTFY_TRANSPORT=stdio ntfy-mcp
// (stdio transport, the usual way for MCP clients to spawn a local server)
package main

import (
	"bufio"
	"bytes"
	"container/list"
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
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var Version = "dev"

// Size and count limits.
const (
	maxMCPBodyBytes  = 1 << 20 // cap on an MCP request body
	maxMessageBytes  = 4096    // ntfy limit for a notification body
	maxTitleBytes    = 512     // cap on the Title header sent upstream
	maxTagsBytes     = 512     // cap on the Tags header sent upstream
	maxClickBytes    = 2048    // cap on the Click header (a URL) sent upstream
	maxIconBytes     = 2048    // cap on the Icon header (a URL) sent upstream
	maxDelayBytes    = 64      // cap on the Delay header sent upstream
	maxEmailBytes    = 254     // RFC 5321 maximum length of an email address
	maxFetchBytes    = 4 << 20 // cap on bytes read from an upstream stream
	defaultReadLimit = 100     // read_notifications limit when omitted
	maxReadLimit     = 1000    // hard cap on read_notifications limit
	guardMaxEntries  = 10000   // cap on tracked targets / client IPs in the guards
)

// httpClient is shared by all upstream requests.
var httpClient *http.Client

// newHTTPClient builds the shared upstream client. Unless
// allowPrivateUpstream is set, the dialer refuses loopback, private,
// link-local, CGNAT, multicast and unspecified IPs, and dials the
// validated address directly.
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

// safeDialContext returns a DialContext that validates the target IP first.
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

// checkDefaultUpstreamAllowed reports an error when the default upstream
// host is an IP, or resolves to an IP, that the dial guard would refuse.
// A DNS lookup failure is not an error: the dial-time guard still applies
// per request.
func checkDefaultUpstreamAllowed(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedUpstreamIP(ip) {
			return fmt.Errorf("invalid NTFY_URL: %s is a private/loopback address but MCP_NTFY_ALLOW_PRIVATE_UPSTREAM is not set (set it to 1 for a LAN/localhost ntfy)", ip)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil
	}
	for _, a := range ips {
		if isBlockedUpstreamIP(a.IP) {
			return fmt.Errorf("invalid NTFY_URL: host %q resolves to %s, a private/loopback address, but MCP_NTFY_ALLOW_PRIVATE_UPSTREAM is not set (set it to 1 for a LAN/localhost ntfy)", host, a.IP)
		}
	}
	return nil
}

// isBlockedUpstreamIP reports whether ip is off-limits as an upstream target.
func isBlockedUpstreamIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true // CGNAT 100.64.0.0/10
	}
	// NAT64/464XLAT well-known prefix 64:ff9b::/96.
	if ip16 := ip.To16(); ip16 != nil && ip16[0] == 0x00 && ip16[1] == 0x64 && ip16[2] == 0xff && ip16[3] == 0x9b {
		return true
	}
	return false
}

// toolInputError marks an error whose message may be returned to the MCP
// client; any other error stays server-side.
type toolInputError struct{ msg string }

func (e *toolInputError) Error() string { return e.msg }

// config is loaded once at startup.
type config struct {
	BaseURL              string // Base URL, already normalized (no trailing slash)
	Topic                string
	Token                string
	Transport            string // "http" or "stdio"
	MCPAddr              string // http transport only
	MCPPort              int    // http transport only
	ClientToken          string // Shared secret MCP clients must present; empty = no client auth
	UnsafeLogs           string
	AllowPrivateUpstream bool // MCP_NTFY_ALLOW_PRIVATE_UPSTREAM=1: allow dialing private/loopback IPs upstream
	Guard                *upstreamGuard
	ClientLimiter        *rateLimiter   // per-IP rate limit on MCP requests; nil = disabled
	AuthFailLimiter      *rateLimiter   // per-IP throttle on failed client auth; nil = disabled
	TrustedProxies       []netip.Prefix // reverse proxies whose X-Forwarded-For is trusted for the per-IP limits
	AllowedHosts         map[string]bool // MCP_NTFY_ALLOWED_HOSTS: public Host values accepted on /mcp (rewritten to the loopback listen address)
	upstreamSem          chan struct{}   // bounds concurrent upstream calls
}

// lruMap is a bounded map with O(1) amortized LRU eviction. Callers must
// hold the owning mutex.
type lruMap[V any] struct {
	maxEntries int
	items      map[string]*list.Element
	lru        *list.List // front = most recently used; Value is *lruEntry[V]
}

type lruEntry[V any] struct {
	key string
	val V
}

func newLRUMap[V any](maxEntries int) *lruMap[V] {
	return &lruMap[V]{
		maxEntries: maxEntries,
		items:      map[string]*list.Element{},
		lru:        list.New(),
	}
}

func (l *lruMap[V]) get(key string) (V, bool) {
	if el, ok := l.items[key]; ok {
		l.lru.MoveToFront(el)
		return el.Value.(*lruEntry[V]).val, true
	}
	var zero V
	return zero, false
}

func (l *lruMap[V]) set(key string, val V) {
	if el, ok := l.items[key]; ok {
		el.Value.(*lruEntry[V]).val = val
		l.lru.MoveToFront(el)
		return
	}
	l.items[key] = l.lru.PushFront(&lruEntry[V]{key: key, val: val})
	if l.lru.Len() > l.maxEntries {
		if back := l.lru.Back(); back != nil {
			l.lru.Remove(back)
			delete(l.items, back.Value.(*lruEntry[V]).key)
		}
	}
}

func (l *lruMap[V]) delete(key string) {
	if el, ok := l.items[key]; ok {
		l.lru.Remove(el)
		delete(l.items, key)
	}
}

// upstreamGuard temporarily blocks an upstream target that keeps answering
// 401/403/429. Keyed by canonical target (scheme://host[:port]).
type upstreamGuard struct {
	mu           sync.Mutex
	maxFailures  int
	window       time.Duration
	blockFor     time.Duration
	rejections   *lruMap[[]time.Time] // target -> recent rejection times
	blockedUntil *lruMap[time.Time]   // target -> end of the block
}

func newUpstreamGuard(maxFailures, windowSec, blockSec int) *upstreamGuard {
	if maxFailures <= 0 {
		return nil // disabled
	}
	return &upstreamGuard{
		maxFailures:  maxFailures,
		window:       time.Duration(windowSec) * time.Second,
		blockFor:     time.Duration(blockSec) * time.Second,
		rejections:   newLRUMap[[]time.Time](guardMaxEntries),
		blockedUntil: newLRUMap[time.Time](guardMaxEntries),
	}
}

// allow returns the remaining block time for target (0 = allowed).
func (g *upstreamGuard) allow(target string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, blocked := g.blockedUntil.get(target)
	if !blocked {
		return 0
	}
	if remaining := time.Until(until); remaining > 0 {
		return remaining
	}
	g.blockedUntil.delete(target)
	return 0
}

// noteRejection records an upstream status and blocks the target once
// maxFailures rejections (401/403/429) land within the window.
func (g *upstreamGuard) noteRejection(target string, status int) {
	if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-g.window)
	times, _ := g.rejections.get(target)
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	if len(kept) >= g.maxFailures {
		g.blockedUntil.set(target, now.Add(g.blockFor))
		g.rejections.delete(target)
		log.Printf("upstream target %q BLOCKED for %s after %d rejections (HTTP %d) within %s",
			target, g.blockFor, g.maxFailures, status, g.window)
		return
	}
	g.rejections.set(target, kept)
}

// checkUpstreamAllowed refuses a blocked target before any upstream call.
func (c config) checkUpstreamAllowed(baseURL string) error {
	if c.Guard == nil {
		return nil
	}
	target := canonicalTarget(baseURL)
	if remaining := c.Guard.allow(target); remaining > 0 {
		log.Printf("BLOCKED request to %q: target temporarily blocked after repeated upstream rejections (retry in %s)",
			target, remaining.Round(time.Second))
		return &toolInputError{fmt.Sprintf("target %s is temporarily blocked after repeated rejected requests (upstream 401/403/429); retry in %s",
			target, remaining.Round(time.Second))}
	}
	return nil
}

// noteUpstreamStatus feeds an upstream response status to the guard.
func (c config) noteUpstreamStatus(baseURL string, status int) {
	if c.Guard == nil {
		return
	}
	c.Guard.noteRejection(canonicalTarget(baseURL), status)
}

// loadConfig reads the environment and validates the result.
func loadConfig() (config, error) {
	var cfg config

	mcpport, err := getenvInt("MCP_NTFY_PORT", "12120")
	if err != nil {
		return cfg, err
	}
	if mcpport < 1 || mcpport > 65535 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_PORT: %d is not a valid port (1-65535)", mcpport)
	}

	maxFail, err := getenvInt("MCP_NTFY_RATE_LIMIT_FAILURES", "5")
	if err != nil {
		return cfg, err
	}
	if maxFail < 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_RATE_LIMIT_FAILURES: %d (0 disables the guard, negative is invalid)", maxFail)
	}
	windowSec, err := getenvInt("MCP_NTFY_RATE_LIMIT_WINDOW", "60")
	if err != nil {
		return cfg, err
	}
	if windowSec <= 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_RATE_LIMIT_WINDOW: %d (must be > 0)", windowSec)
	}
	blockSec, err := getenvInt("MCP_NTFY_RATE_LIMIT_BLOCK", "300")
	if err != nil {
		return cfg, err
	}
	if blockSec <= 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_RATE_LIMIT_BLOCK: %d (must be > 0)", blockSec)
	}
	clientRateLimit, err := getenvInt("MCP_NTFY_CLIENT_RATE_LIMIT", "120")
	if err != nil {
		return cfg, err
	}
	if clientRateLimit < 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_CLIENT_RATE_LIMIT: %d (0 disables the limit, negative is invalid)", clientRateLimit)
	}
	authFailLimit, err := getenvInt("MCP_NTFY_AUTH_FAILURE_LIMIT", "10")
	if err != nil {
		return cfg, err
	}
	if authFailLimit < 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_AUTH_FAILURE_LIMIT: %d (0 disables the limit, negative is invalid)", authFailLimit)
	}
	maxConcurrent, err := getenvInt("MCP_NTFY_MAX_CONCURRENT_UPSTREAM", "32")
	if err != nil {
		return cfg, err
	}
	if maxConcurrent <= 0 {
		return cfg, fmt.Errorf("invalid MCP_NTFY_MAX_CONCURRENT_UPSTREAM: %d (must be > 0)", maxConcurrent)
	}
	allowPrivate := isTruthy(getenv("MCP_NTFY_ALLOW_PRIVATE_UPSTREAM", "0"))

	trustedProxies, err := parseTrustedProxies(getenv("MCP_NTFY_TRUSTED_PROXIES", ""))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_TRUSTED_PROXIES environment variable: %w", err)
	}
	allowedHosts, err := parseAllowedHosts(getenv("MCP_NTFY_ALLOWED_HOSTS", ""))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_ALLOWED_HOSTS environment variable: %w", err)
	}

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
		ClientLimiter:        newRateLimiter(clientRateLimit, windowSec, guardMaxEntries),
		AuthFailLimiter:      newRateLimiter(authFailLimit, windowSec, guardMaxEntries),
		TrustedProxies:       trustedProxies,
		AllowedHosts:         allowedHosts,
		upstreamSem:          make(chan struct{}, maxConcurrent),
	}

	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return cfg, fmt.Errorf("invalid NTFY_URL: %w", err)
	}
	if !topicRe.MatchString(cfg.Topic) {
		return cfg, fmt.Errorf("invalid NTFY_TOPIC %q: must be 1-64 chars of [A-Za-z0-9_-]", cfg.Topic)
	}
	if ip := net.ParseIP(strings.Trim(cfg.MCPAddr, "[]")); ip == nil && !hostnameRe.MatchString(cfg.MCPAddr) {
		return cfg, fmt.Errorf("invalid MCP_NTFY_ADDR %q: not an IP address or hostname", cfg.MCPAddr)
	}
	// net.Listen needs an IPv6 literal in brackets ("[::1]:port").
	if ip := net.ParseIP(strings.Trim(cfg.MCPAddr, "[]")); ip != nil && strings.Contains(cfg.MCPAddr, ":") && !strings.HasPrefix(cfg.MCPAddr, "[") {
		cfg.MCPAddr = "[" + cfg.MCPAddr + "]"
	}
	if !allowPrivate {
		if err := checkDefaultUpstreamAllowed(cfg.BaseURL); err != nil {
			return cfg, err
		}
	}
	for _, key := range []string{"MCP_NTFY_ALLOW_PRIVATE_UPSTREAM", "MCP_NTFY_UNSAFE_LOGS"} {
		switch v := getenv(key, "0"); {
		case v == "0" || v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "false"):
		default:
			log.Printf("WARNING: unrecognized %s=%q (want 0 or 1); treating it as false", key, v)
		}
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
	if len(trustedProxies) > 0 {
		log.Printf("trusted proxies for X-Forwarded-For (per-client rate limits): %v", trustedProxies)
	}
	if len(allowedHosts) > 0 {
		hosts := make([]string, 0, len(allowedHosts))
		for h := range allowedHosts {
			hosts = append(hosts, h)
		}
		log.Printf("MCP_NTFY_ALLOWED_HOSTS: these public Host headers are accepted on /mcp (rewritten to the loopback listen address): %q", hosts)
	}

	if cfg.Guard != nil {
		log.Printf("upstream anti-DoS guard enabled: %d rejections (401/403/429) within %ds block a target for %ds",
			maxFail, windowSec, blockSec)
	}

	if cfg.ClientToken != "" {
		log.Print("MCP client authentication enabled: requests must carry the MCP_NTFY_CLIENT_TOKEN as a Bearer token")
	} else if transport == "http" && isLoopbackAddr(cfg.MCPAddr) {
		log.Print("MCP client authentication disabled (MCP_NTFY_CLIENT_TOKEN is empty)")
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

// getenvInt parses an integer environment variable.
func getenvInt(key, fallback string) (int, error) {
	v, err := strconv.Atoi(getenv(key, fallback))
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return v, nil
}

func isTruthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
}

// parseTrustedProxies parses a comma-separated list of proxy IPs or CIDRs
// whose X-Forwarded-For header may be trusted for per-client rate limits.
func parseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
			}
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("invalid IP %q: %w", part, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// parseAllowedHosts parses a comma-separated list of Host values (with or
// without port) into a set of lowercase hostnames. Empty input returns nil.
func parseAllowedHosts(s string) (map[string]bool, error) {
	var out map[string]bool
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(part); err == nil {
			part = host
		}
		part = strings.ToLower(strings.Trim(part, "[]"))
		if net.ParseIP(part) == nil && !hostnameRe.MatchString(part) {
			return nil, fmt.Errorf("invalid Host value %q: not an IP or hostname", part)
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[part] = true
	}
	return out, nil
}

// topicRe is the charset ntfy accepts for a topic.
var topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// hostnameRe matches a hostname (IPs are checked separately with net.ParseIP).
var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// delayRe is the charset ntfy accepts in the Delay header (relative
// durations like "30min" or absolute timestamps like "2025-12-31 18:00").
var delayRe = regexp.MustCompile(`^[0-9A-Za-z ,.:+-]{1,64}$`)

// emailRe matches a plain address for the Email (forward) header.
var emailRe = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]{1,190}\.[^@\s]{1,63}$`)

// validatePublicURL checks that raw is an absolute http(s) URL without
// credentials, for the click and icon fields.
func validatePublicURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return &toolInputError{fmt.Sprintf("%s must be an absolute http(s) URL without embedded credentials", field)}
	}
	return nil
}

// validateBaseURL accepts only absolute http(s) URLs without credentials.
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

// canonicalTarget reduces a base URL to scheme://host[:port], the key used
// by the upstream guard. The path is not part of the key.
func canonicalTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return scheme + "://" + authority
}

// isLoopbackAddr reports whether the listen address is loopback.
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

// rateLimiter is a bounded per-key sliding-window counter.
type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   *lruMap[[]time.Time]
}

func newRateLimiter(max, windowSec, maxEntries int) *rateLimiter {
	if max <= 0 {
		return nil // disabled
	}
	return &rateLimiter{
		max:    max,
		window: time.Duration(windowSec) * time.Second,
		hits:   newLRUMap[[]time.Time](maxEntries),
	}
}

// allow records a hit for key and reports whether it is within the limit.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	times, _ := l.hits.get(key)
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.hits.set(key, kept)
	return len(kept) <= l.max
}

// clientIP returns the key used for per-client rate limits: the last
// X-Forwarded-For entry when the direct peer is a trusted proxy (see
// MCP_NTFY_TRUSTED_PROXIES) and that entry is a valid IP, otherwise the
// direct peer IP.
// peerHost returns the IP part of the request's remote address.
func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host := peerHost(r)
	if addr, err := netip.ParseAddr(host); err == nil {
		for _, p := range trusted {
			if p.Contains(addr) {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					entries := strings.Split(xff, ",")
					last := strings.TrimSpace(entries[len(entries)-1])
					if _, err := netip.ParseAddr(last); err == nil {
						return last
					}
				}
				break
			}
		}
	}
	return host
}

// trustedPeerWithoutXFF reports whether the request comes from a trusted
// proxy but carries no usable X-Forwarded-For header, so rate limits fall
// back to the proxy IP (one bucket shared by all clients behind it).
func trustedPeerWithoutXFF(r *http.Request, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(peerHost(r))
	if err != nil {
		return false
	}
	for _, p := range trusted {
		if !p.Contains(addr) {
			continue
		}
		xff := r.Header.Get("X-Forwarded-For")
		if xff == "" {
			return true
		}
		entries := strings.Split(xff, ",")
		last := strings.TrimSpace(entries[len(entries)-1])
		_, err = netip.ParseAddr(last)
		return err != nil
	}
	return false
}

// withRateLimit answers 429 past the per-IP limit and caps the request body.
func withRateLimit(limiter *rateLimiter, trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trustedPeerWithoutXFF(r, trusted) {
			log.Printf("WARNING: request from trusted proxy %s without usable X-Forwarded-For: per-client rate limits use the proxy IP (all clients share one bucket)", peerHost(r))
		}
		if !limiter.allow(clientIP(r, trusted)) {
			log.Printf("RATE LIMITED request from %s to %s %q: too many requests", r.RemoteAddr, r.Method, r.URL.Path)
			http.Error(w, "too many requests, retry later", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether two base URLs share scheme, host and path;
// unparseable URLs or userinfo count as a different origin.
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

// withClientAuth requires "Authorization: Bearer <secret>" (an empty
// secret disables the check). Both sides are hashed and compared in
// constant time; failures are throttled per IP and the presented token
// is never logged.
func withClientAuth(secret string, failLimiter *rateLimiter, trusted []netip.Prefix, next http.Handler) http.Handler {
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
			if !failLimiter.allow(clientIP(r, trusted)) {
				log.Printf("THROTTLED MCP request from %s to %s %q: too many failed authentication attempts", r.RemoteAddr, r.Method, r.URL.Path)
				http.Error(w, "too many failed authentication attempts, retry later", http.StatusTooManyRequests)
				return
			}
			log.Printf("REJECTED MCP request from %s to %s %q: missing or invalid client token", r.RemoteAddr, r.Method, r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="ntfy-mcp"`)
			http.Error(w, "unauthorized: a valid Bearer token is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAllowedHosts rewrites the Host header of requests whose Host matches
// the configured allowlist to the loopback listen address. The MCP transport
// rejects non-loopback Host headers on loopback connections (DNS-rebinding
// guard), which breaks reverse proxies that preserve the public Host; this
// lets exactly the configured public names through, any other Host still
// hits the guard.
func withAllowedHosts(allowed map[string]bool, loopbackHost string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if allowed[strings.ToLower(strings.Trim(host, "[]"))] {
			r2 := r.Clone(r.Context())
			r2.Host = loopbackHost
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notificationArgs are the send_notification parameters.
type notificationArgs struct {
	BaseURL  string `json:"base_url,omitempty"`
	Token    string `json:"token,omitempty"`
	Message  string `json:"message"`
	Title    string `json:"title,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Tags     string `json:"tags,omitempty"`  // Comma-separated (e.g., "warning,phone")
	Click    string `json:"click,omitempty"` // URL opened when the notification is clicked
	Icon     string `json:"icon,omitempty"`  // URL of an icon shown with the notification
	Delay    string `json:"delay,omitempty"` // Delivery delay, e.g. "30min" or "2025-12-31 18:00"
	Email    string `json:"email,omitempty"` // Email address the notification is forwarded to
	Markdown bool   `json:"markdown,omitempty"`
}

func parseArgs[T any](request mcp.CallToolRequest) (T, error) {
	var args T
	raw, err := json.Marshal(request.GetArguments())
	if err != nil {
		return args, &toolInputError{fmt.Sprintf("invalid arguments: %v", err)}
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, &toolInputError{fmt.Sprintf("invalid arguments: %v", err)}
	}
	return args, nil
}

// toolErrorResult maps an error to an MCP tool result: toolInputError
// messages reach the client, any other error is logged server-side and
// replaced by a generic message.
func toolErrorResult(err error, generic string) *mcp.CallToolResult {
	var ie *toolInputError
	if errors.As(err, &ie) {
		return mcp.NewToolResultError(ie.Error())
	}
	log.Printf("ERROR: %v", err)
	return mcp.NewToolResultError(generic)
}

func (c config) publish(ctx context.Context, args notificationArgs) error {

	// Applies defaults; a custom base_url needs its own token in the request.
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
	if len(args.Title) > maxTitleBytes {
		return &toolInputError{fmt.Sprintf("title exceeds the %d-byte limit", maxTitleBytes)}
	}
	if len(args.Tags) > maxTagsBytes {
		return &toolInputError{fmt.Sprintf("tags exceed the %d-byte limit", maxTagsBytes)}
	}
	if len(args.Click) > maxClickBytes {
		return &toolInputError{fmt.Sprintf("click exceeds the %d-byte limit", maxClickBytes)}
	}
	if args.Click != "" {
		if err := validatePublicURL("click", args.Click); err != nil {
			return err
		}
	}
	if len(args.Icon) > maxIconBytes {
		return &toolInputError{fmt.Sprintf("icon exceeds the %d-byte limit", maxIconBytes)}
	}
	if args.Icon != "" {
		if err := validatePublicURL("icon", args.Icon); err != nil {
			return err
		}
	}
	if len(args.Delay) > maxDelayBytes {
		return &toolInputError{fmt.Sprintf("delay exceeds the %d-byte limit", maxDelayBytes)}
	}
	if args.Delay != "" && !delayRe.MatchString(args.Delay) {
		return &toolInputError{"invalid delay: use a relative duration like \"30min\" or an absolute time like \"2025-12-31 18:00\""}
	}
	if len(args.Email) > maxEmailBytes {
		return &toolInputError{fmt.Sprintf("email exceeds the %d-byte limit", maxEmailBytes)}
	}
	if args.Email != "" && !emailRe.MatchString(args.Email) {
		return &toolInputError{"invalid email: expected a plain address like \"ops@example.com\""}
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

	// Tokens are redacted from logs unless MCP_NTFY_UNSAFE_LOGS=1.
	logToken := "provided=%t"
	tokenArgs := any(token != "")
	if c.UnsafeLogs == "1" {
		logToken = "%q"
		tokenArgs = token
	}
	log.Printf("publishing: POST %q/%q (title=%q, priority=%d, tags=%q, click=%q, icon=%q, delay=%q, email=%q, markdown=%t, token "+logToken+")",
		baseURL, topic, args.Title, args.Priority, args.Tags, args.Click, args.Icon, args.Delay, args.Email, args.Markdown, tokenArgs)

	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	req.Header.Set("Title", args.Title)

	if args.Priority != 0 {
		req.Header.Set("Priority", strconv.Itoa(args.Priority))
	}
	if args.Tags != "" {
		req.Header.Set("Tags", args.Tags)
	}
	if args.Click != "" {
		req.Header.Set("Click", args.Click)
	}
	if args.Icon != "" {
		req.Header.Set("Icon", args.Icon)
	}
	if args.Delay != "" {
		req.Header.Set("Delay", args.Delay)
	}
	if args.Email != "" {
		req.Header.Set("Email", args.Email)
	}
	if args.Markdown {
		req.Header.Set("Markdown", "yes")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("ERROR sending to ntfy: %q", err.Error())
		var ie *toolInputError
		if errors.As(err, &ie) {
			return ie // dial-time refusal of a private/loopback target
		}
		return fmt.Errorf("error sending to ntfy: %w", err)
	}
	log.Printf("ntfy response: HTTP status code %d", resp.StatusCode)
	c.noteUpstreamStatus(baseURL, resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The upstream body stays server-side.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("ERROR ntfy response body: %q", strings.TrimSpace(string(body)))
		return fmt.Errorf("ntfy responded with status code %d", resp.StatusCode)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) //nolint:errcheck
	return nil
}

// acquireUpstreamSlot bounds concurrent upstream calls.
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

// readArgs are the read_notifications parameters.
type readArgs struct {
	BaseURL    string `json:"base_url,omitempty"`
	Token      string `json:"token,omitempty"`
	Topic      string `json:"topic,omitempty"`
	Since      string `json:"since,omitempty"` // "all", a unix timestamp, or a notification ID
	Limit      int    `json:"limit,omitempty"` // Max number of notifications to return (client-side cap)
	CachedOnly bool   `json:"cached_only,omitempty"`
}

// ntfyNotification is one JSON object from the ntfy stream (one per line).
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
	Delay    string   `json:"delay,omitempty"`
	Email    string   `json:"email,omitempty"`
	Markdown bool     `json:"markdown,omitempty"`
}

// resolveTarget applies server-side defaults: a custom base_url requires
// a request token, a valid http(s) URL and a valid topic.
func (c config) resolveTarget(baseURL, token, topic string) (string, string, string, error) {
	if baseURL == "" {
		baseURL = c.BaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL != c.BaseURL && !sameOrigin(baseURL, c.BaseURL) && token == "" {
		log.Printf("REJECTED custom base_url %q without request token: server token must not leave to an agent-chosen URL", baseURL)
		return "", "", "", &toolInputError{"custom base_url requires a token in the request"}
	}
	// Only absolute http(s) URLs without embedded credentials.
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

// fetch returns the cached notifications of a topic; poll=1 makes the
// server answer immediately instead of streaming new ones.
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

	// Default and cap the limit, and cap the total bytes read.
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

	// Same log redaction as publish: tokens stay out of the logs unless
	// MCP_NTFY_UNSAFE_LOGS=1.
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
			return nil, ie // dial-time refusal of a private/loopback target
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

	// One JSON object per line; stop at the limit or the byte cap.
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

// countingReader counts the bytes read from an upstream stream.
type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// buildNotificationTool declares the send_notification tool.
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
		mcp.WithString("click",
			mcp.Description("URL opened when the notification is clicked (http or https only)."),
		),
		mcp.WithString("icon",
			mcp.Description("URL of an icon shown with the notification (http or https only)."),
		),
		mcp.WithString("delay",
			mcp.Description("Deliver the notification later: a relative duration like \"30min\" or an absolute time like \"2025-12-31 18:00\"."),
		),
		mcp.WithString("email",
			mcp.Description("Email address the notification is also forwarded to (requires server-side email config)."),
		),
		mcp.WithBoolean("markdown",
			mcp.Description("Render the message as Markdown in clients that support it."),
		),
	)
}

// notificationHandler handles send_notification calls.
func notificationHandler(cfg config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs[notificationArgs](request)
		if err != nil {
			return toolErrorResult(err, "invalid request"), nil
		}

		// Log the request with the token redacted and %q on client fields.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("send_notification called with: base_url=%q topic=%q title=%q priority=%d tags=%q click=%q icon=%q delay=%q email=%q markdown=%t message_len=%d token=%q",
			logged.BaseURL, logged.Topic, logged.Title, logged.Priority, logged.Tags, logged.Click, logged.Icon, logged.Delay, logged.Email, logged.Markdown, len(args.Message), logged.Token)

		if strings.TrimSpace(args.Message) == "" {
			log.Print("ERROR: message parameter is empty")
			return mcp.NewToolResultError("message parameter cannot be empty"), nil
		}

		if err := cfg.publish(ctx, args); err != nil {
			return toolErrorResult(err, "failed to send the notification to the upstream ntfy server"), nil
		}
		log.Printf("notification published successfully")
		return mcp.NewToolResultText("Notification sent successfully."), nil
	}
}

// buildReadTool declares the read_notifications tool.
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

// readHandler handles read_notifications calls.
func readHandler(cfg config) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := parseArgs[readArgs](request)
		if err != nil {
			return toolErrorResult(err, "invalid request"), nil
		}

		// Same log redaction as send_notification.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("read_notifications called with: base_url=%q topic=%q since=%q limit=%d cached_only=%t token=%q",
			logged.BaseURL, logged.Topic, logged.Since, logged.Limit, logged.CachedOnly, logged.Token)

		notifs, err := cfg.fetch(ctx, args)
		if err != nil {
			return toolErrorResult(err, "failed to read notifications from the upstream ntfy server"), nil
		}
		if len(notifs) == 0 {
			log.Printf("no notifications found")
			return mcp.NewToolResultText("No notifications found."), nil
		}
		out, err := json.MarshalIndent(notifs, "", "  ")
		if err != nil {
			log.Printf("ERROR encoding notifications: %v", err)
			return mcp.NewToolResultError("failed to encode the notifications"), nil
		}
		log.Printf("returning %d notification(s)", len(notifs))
		return mcp.NewToolResultText(string(out)), nil
	}
}

// healthzHandler answers liveness/readiness probes with a static 200.
// It requires the Bearer token like every other endpoint when
// MCP_NTFY_CLIENT_TOKEN is set, but is not rate-limited.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
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
		// Stdio transport: JSON-RPC on stdin/stdout; logs stay on stderr.
		log.Printf("server v.%s serving MCP over stdio", Version)
		if err := server.ServeStdio(srv); err != nil {
			log.Fatalf("fatal error: %v", err)
		}
		return
	}

	transport := server.NewStreamableHTTPServer(srv)

	// /mcp is rate-limited per client IP and its body capped; /healthz is
	// not rate-limited. Both endpoints require the Bearer token when
	// MCP_NTFY_CLIENT_TOKEN is set.
	loopbackHost := cfg.MCPAddr
	if host, _, err := net.SplitHostPort(cfg.MCPAddr); err == nil {
		loopbackHost = host
	}
	loopbackHost = strings.Trim(loopbackHost, "[]")

	mux := http.NewServeMux()
	mux.Handle("/mcp", withAllowedHosts(cfg.AllowedHosts, loopbackHost, withRateLimit(cfg.ClientLimiter, cfg.TrustedProxies, transport)))
	mux.HandleFunc("/healthz", healthzHandler)

	addr := fmt.Sprintf("%s:%d", cfg.MCPAddr, cfg.MCPPort)
	httpSrv := &http.Server{
		Addr: addr,
		Handler: withClientAuth(cfg.ClientToken, cfg.AuthFailLimiter, cfg.TrustedProxies, mux),
		// Server timeouts.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	// SIGTERM/SIGINT trigger a graceful shutdown: stop accepting new
	// connections, let in-flight requests finish (bounded by the timeout
	// below), then exit 0.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	log.Printf("server v.%s listening on %s (MCP path /mcp, health endpoint /healthz)", Version, addr)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fatal error: %v", err)
		}
	case <-sigCtx.Done():
		log.Print("shutdown signal received: closing listeners, draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("error during graceful shutdown: %v", err)
		}
		log.Print("graceful shutdown complete")
	}
}

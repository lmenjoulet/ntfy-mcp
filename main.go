// MCP server exposing tools to send and read notifications on an ntfy server.
//
// Configuration via environment variables:
//
//	NTFY_URL      Default ntfy server URL (default: https://ntfy.sh)
//	NTFY_TOPIC    Default topic (default: agent)
//	NTFY_TOKEN    Default token (default: empty)
//
//	MCP_NTFY_ADDR MCP server listen address (default: 127.0.0.1)
//	MCP_NTFY_PORT MCP server port (default: 12120)
//
//	MCP_NTFY_UNSAFE_LOGS  When set to "1", authentication tokens are logged
//	                      in cleartext (unsafe, debugging only; default: off)
//
// An agent can override any of these variables in an MCP request.
// Configure them according to your security model.
// Example: Public MCP with known token, or private MCP.
// Usage: ntfy-mcp (communicates over stdin/stdout)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var Version = "dev"

// httpClient is shared across all requests: its TCP connections are
// reused between calls (keep-alive), avoiding a full TLS handshake per notification.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	},
}

// config holds the configuration loaded once at startup.
type config struct {
	BaseURL    string // Base URL, already normalized (no trailing slash)
	Topic      string
	Token      string
	MCPAddr    string
	MCPPort    int
	UnsafeLogs string
}

func loadConfig() (config, error) {
	var cfg config

	mcpport, err := strconv.Atoi(getenv("MCP_NTFY_PORT", "12120"))
	if err != nil {
		return cfg, fmt.Errorf("error getting MCP_NTFY_PORT environment variable: %w", err)
	}

	cfg = config{
		BaseURL:    strings.TrimRight(getenv("NTFY_URL", "https://ntfy.sh"), "/"),
		Topic:      getenv("NTFY_TOPIC", "agent"),
		Token:      getenv("NTFY_TOKEN", ""),
		MCPAddr:    getenv("MCP_NTFY_ADDR", "127.0.0.1"),
		MCPPort:    mcpport,
		UnsafeLogs: getenv("MCP_NTFY_UNSAFE_LOGS", "0"),
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/"+topic, strings.NewReader(args.Message))
	if err != nil {
		log.Printf("ERROR building request to %q: %v", baseURL+"/"+topic, err)
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
	log.Printf("publishing: POST %s/%s (title=%q, priority=%d, tags=%q, token "+logToken+")",
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
		log.Printf("ERROR sending to ntfy: %v", err)
		return fmt.Errorf("error sending to ntfy: %w", err)
	}
	log.Printf("ntfy response: HTTP status code %d", resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("ERROR ntfy response body: %s", strings.TrimSpace(string(body)))
		return fmt.Errorf("ntfy responded with status code %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) //nolint:errcheck
	return nil
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
		return "", "", "", fmt.Errorf("custom base_url requires a token in the request")
	}
	if token == "" {
		token = c.Token
	}
	if topic == "" {
		topic = c.Topic
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
		log.Printf("ERROR building request to %q: %v", baseURL+"/"+topic, err)
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
	log.Printf("reading: GET %s/%s (since=%q, cached_only=%t, limit=%d, token "+logToken+")",
		baseURL, topic, args.Since, args.CachedOnly, args.Limit, tokenArgs)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("ERROR reading from ntfy: %v", err)
		return nil, fmt.Errorf("error reading from ntfy: %w", err)
	}
	log.Printf("ntfy response: HTTP status code %d", resp.StatusCode)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("ERROR ntfy response body: %s", strings.TrimSpace(string(body)))
		return nil, fmt.Errorf("ntfy responded with status code %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// ntfy streams one JSON object per line; parse them as they arrive
	// and stop early once the requested limit is reached.
	var notifs []ntfyNotification
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var n ntfyNotification
		if err := json.Unmarshal(line, &n); err != nil {
			log.Printf("WARNING skipping undecodable ntfy line: %v", err)
			continue
		}
		notifs = append(notifs, n)
		if args.Limit > 0 && len(notifs) >= args.Limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("ERROR reading ntfy stream: %v", err)
		return nil, fmt.Errorf("error reading ntfy stream: %w", err)
	}
	return notifs, nil
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
		// opts in to cleartext token logging, for debugging only.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("send_notification called with: %+v", logged)

		if strings.TrimSpace(args.Message) == "" {
			log.Print("ERROR: message parameter is empty")
			return nil, fmt.Errorf("message parameter cannot be empty")
		}

		if err := cfg.publish(ctx, args); err != nil {
			log.Printf("ERROR publishing failed: %v", err)
			return mcp.NewToolResultError(err.Error()), nil
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

		// Same redaction policy as send_notification.
		logged := args
		if cfg.UnsafeLogs != "1" {
			logged.Token = "[redacted]"
		}
		log.Printf("read_notifications called with: %+v", logged)

		notifs, err := cfg.fetch(ctx, args)
		if err != nil {
			log.Printf("ERROR reading failed: %v", err)
			return mcp.NewToolResultError(err.Error()), nil
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

	srv := server.NewMCPServer(
		"ntfy-mcp",
		Version,
		server.WithToolCapabilities(false),
	)

	srv.AddTool(buildNotificationTool(), notificationHandler(cfg))
	srv.AddTool(buildReadTool(), readHandler(cfg))

	httpSrv := server.NewStreamableHTTPServer(srv)
	addr := fmt.Sprintf("%s:%d", cfg.MCPAddr, cfg.MCPPort)
	log.Printf("server v.%s listening on %s (path /mcp)", Version, addr)
	if err := httpSrv.Start(addr); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

// MCP server exposing a tool to send notifications to an ntfy server.
//
// Configuration via environment variables:
//
//  NTFY_URL      Default ntfy server URL (default: https://ntfy.sh)
//  NTFY_TOPIC    Default topic (default: agent)
//  NTFY_TOKEN    Default token (default: empty)
//
//  MCP_NTFY_ADDR MCP server listen address (default: 127.0.0.1)
//  MCP_NTFY_PORT MCP server port (default: 12120)
//
// An agent can override any of these variables in an MCP request.
// Configure them according to your security model.
// Example: Public MCP with known token, or private MCP.
// Usage: ntfy-mcp (communicates over stdin/stdout)
package main

import (
  "context"
  "encoding/json"
  "fmt"
  "io"
  "net"
  "net/http"
  "net/url"
  "log"
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
  BaseURL string // Base URL, already normalized (no trailing slash)
  Topic   string
  Token   string
  MCPAddr string
  MCPPort int
}

func loadConfig() (config, error) {
  var cfg config

  mcpport, err := strconv.Atoi(getenv("MCP_NTFY_PORT", "12120"))
  if err != nil {
      return cfg, fmt.Errorf("error getting MCP_NTFY_PORT environment variable: %w", err)
  }

  cfg = config {
    BaseURL: strings.TrimRight(getenv("NTFY_URL", "https://ntfy.sh"), "/"),
    Topic:   getenv("NTFY_TOPIC", "agent"),
    Token:   getenv("NTFY_TOKEN", ""),
    MCPAddr: getenv("MCP_NTFY_ADDR", "127.0.0.1"),
    MCPPort: mcpport,
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

func parseArgs(request mcp.CallToolRequest) (notificationArgs, error) {
  var args notificationArgs
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

  baseURL := args.BaseURL
  if baseURL == "" {
    baseURL = c.BaseURL
  }

  // Anti-exfiltration guard: an agent-chosen base_url is only accepted
  // when the request carries its own token. Otherwise the server's
  // token could be sent to an arbitrary destination. Origins are
  // compared as parsed URLs (scheme + host + path), never as raw
  // strings, so look-alike spellings cannot slip through.
  if args.BaseURL != "" && !sameOrigin(args.BaseURL, c.BaseURL) && args.Token == "" {
    log.Printf("REJECTED custom base_url %q without request token: server token must not leave to an agent-chosen URL", args.BaseURL)
    return fmt.Errorf("custom base_url requires a token in the request")
  }

  topic := args.Topic
  if topic == "" {
    topic = c.Topic
  }

  req, err := http.NewRequestWithContext(ctx, http.MethodPost,
    baseURL+"/"+topic, strings.NewReader(args.Message))
  if err != nil {
    log.Printf("ERROR building request to %q: %v", baseURL+"/"+topic, err)
    return fmt.Errorf("error building request: %w", err)
  }

  token := args.Token
  if token == "" {
    token = c.Token
  }
  if token != "" {
    req.Header.Set("Authorization", "Bearer "+token)
  }

  log.Printf("publishing: POST %s/%s (title=%q, priority=%d, tags=%q, token provided=%t)",
    baseURL, topic, args.Title, args.Priority, args.Tags, token != "")

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
    args, err := parseArgs(request)
    if err != nil {
      log.Printf("ERROR: %v", err)
      return nil, err
    }

    // Log the call with the token redacted: tokens must never reach the logs.
    redacted := args
    redacted.Token = "[redacted]"
    log.Printf("send_notification called with: %+v", redacted)

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

  httpSrv := server.NewStreamableHTTPServer(srv)
  addr := fmt.Sprintf("%s:%d", cfg.MCPAddr, cfg.MCPPort)
  log.Printf("server v.%s listening on %s (path /mcp)", Version, addr)
  if err := httpSrv.Start(addr); err != nil {
    log.Fatalf("fatal error: %v", err)
  }
}

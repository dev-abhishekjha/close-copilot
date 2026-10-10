package agent

// The MCP client tool registry (CC-702). The agent reaches ERPNext and the
// evidence store only through the two read-only MCP servers on /mcp: the
// books server (BOOKS_MCP_URL) and the evidence server (EVIDENCE_MCP_URL).
// NewRegistry connects to both with the agent token, lists their tools,
// refuses to start unless every tool is annotated read-only, and exposes
// them to a model as llm.ToolSpecs named <server>__<tool>. Execute runs a
// model's tool call; the MCP-backed readers in readers.go share the same
// sessions.
//
// The registry never connects to /mcp-admin and never reads the admin
// token. Tool results pass through unchanged (fencing is CC-706/CC-1103),
// and nothing here logs result content: only tool names, durations and
// error classes.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
	"github.com/abhishekjha/close-copilot/internal/llm"
)

// Server names, the prefix of every tool name the model sees.
const (
	ServerBooks    = "books"
	ServerEvidence = "evidence"
)

// toolSep joins a server name and a tool name: books__list_gl_entries.
const toolSep = "__"

// MCPPath is the only endpoint path the registry connects to. The admin
// endpoint (/mcp-admin) carries the write tools and is refused.
const MCPPath = "/mcp"

// DefaultCallTimeout bounds one tool call, a reconnect included.
const DefaultCallTimeout = 10 * time.Second

// Fixed texts of error results. They never carry transport detail, which
// could echo a URL or a header.
const (
	MsgUnknownTool = "error: unknown tool; call only the tools offered"
	MsgBadArgs     = "error: tool arguments are not a JSON object"
	MsgToolError   = "error: the tool reported an error; check the arguments against the tool's schema"
	MsgTimeout     = "error: the tool call timed out"
	MsgUnavailable = "error: the tool server is unavailable"
)

// Error classes the readers wrap; the message carries no transport detail.
var (
	// ErrUnknownTool is wrapped for a tool the registry doesn't offer.
	ErrUnknownTool = errors.New("agent: unknown tool")
	// ErrToolError is wrapped when a tool reports an error (IsError).
	ErrToolError = errors.New("agent: tool reported an error")
	// ErrTimeout is wrapped when a call exceeds its deadline.
	ErrTimeout = errors.New("agent: tool call timed out")
	// ErrUnavailable is wrapped for a transport or protocol failure.
	ErrUnavailable = errors.New("agent: tool server unavailable")
	// ErrBadResult is wrapped when a result isn't the JSON the tool's
	// output schema promises.
	ErrBadResult = errors.New("agent: tool result does not decode")
)

// llmToolName is what the model providers accept as a tool name.
var llmToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Option changes how NewRegistry builds the registry.
type Option func(*options)

type options struct {
	callTimeout     time.Duration
	discoverTimeout time.Duration
}

// WithCallTimeout overrides the per-call timeout (default 10 s). Tests use
// it to exercise timeouts quickly.
func WithCallTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.callTimeout = d
		}
	}
}

// WithDiscoverTimeout overrides the startup deadline per server (default
// 30 s): connecting and listing its tools.
func WithDiscoverTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.discoverTimeout = d
		}
	}
}

// DefaultDiscoverTimeout bounds connecting to one server and listing its
// tools at startup.
const DefaultDiscoverTimeout = 30 * time.Second

// MaxToolsPerServer caps the tools one server may list; a server listing
// more (or paging forever) stops the start.
const MaxToolsPerServer = 64

// maxToolText caps a server-supplied tool name in an error message.
const maxToolText = 40

// Registry holds one MCP session per server and the read-only tools they
// offer. It is safe for concurrent use.
type Registry struct {
	log      *slog.Logger
	timeout  time.Duration
	discover time.Duration
	servers  map[string]*serverConn
	tools    map[string]toolRef // prefixed name -> server and tool
	specs    []llm.ToolSpec     // sorted by name
}

// toolRef names a tool on one server.
type toolRef struct {
	server string
	tool   string
}

// serverConn is one server's session, replaced on reconnect.
type serverConn struct {
	name     string
	endpoint string
	client   *mcp.Client
	http     *http.Client

	mu      sync.Mutex
	session *mcp.ClientSession
	closed  bool
}

// NewRegistry connects to the books and evidence MCP servers named by
// cfg.BooksMCPURL and cfg.EvidenceMCPURL with the agent token, discovers
// their tools and logs the final tool list once. It fails when a URL's
// path is not exactly /mcp, the agent token is empty, a server can't be
// reached or refuses the token, or any tool lacks readOnlyHint: true.
func NewRegistry(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Option) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	o := options{callTimeout: DefaultCallTimeout, discoverTimeout: DefaultDiscoverTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	booksURL, booksKey, err := checkEndpoint(config.EnvBooksMCPURL, cfg.BooksMCPURL)
	if err != nil {
		return nil, err
	}
	evidenceURL, evidenceKey, err := checkEndpoint(config.EnvEvidenceMCPURL, cfg.EvidenceMCPURL)
	if err != nil {
		return nil, err
	}
	token := cfg.MCPTokenAgent.Reveal()
	if token == "" {
		return nil, fmt.Errorf("agent: registry: %s is empty", config.EnvMCPTokenAgent)
	}
	hc, err := httpx.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("agent: registry: http client: %w", err)
	}
	// Never follow a redirect: a 307 from /mcp could point at /mcp-admin
	// or at another allowlisted host. The SDK sees the 3xx as a failure.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Transport = &bearerTransport{
		base:    hc.Transport,
		token:   token,
		allowed: map[string]bool{booksKey: true, evidenceKey: true},
	}

	r := &Registry{
		log:      log,
		timeout:  o.callTimeout,
		discover: o.discoverTimeout,
		servers:  map[string]*serverConn{},
		tools:    map[string]toolRef{},
	}
	for _, s := range []struct{ name, endpoint string }{
		{ServerBooks, booksURL},
		{ServerEvidence, evidenceURL},
	} {
		sc := &serverConn{
			name:     s.name,
			endpoint: s.endpoint,
			http:     hc,
			client:   mcp.NewClient(&mcp.Implementation{Name: "close-copilot-agent", Version: buildinfo.Version}, nil),
		}
		r.servers[s.name] = sc
		if err := r.discoverServer(ctx, sc); err != nil {
			r.Close()
			return nil, err
		}
	}
	slices.SortFunc(r.specs, func(a, b llm.ToolSpec) int { return cmp.Compare(a.Name, b.Name) })

	names := make([]string, len(r.specs))
	for i, s := range r.specs {
		names[i] = s.Name
	}
	log.InfoContext(ctx, "agent tool registry ready", "tools", names, "count", len(names))
	return r, nil
}

// checkEndpoint parses an MCP URL and requires an http(s) URL with a host,
// no credentials, no query or fragment, and the path exactly /mcp. It
// returns the URL and its endpoint key (see endpointKey).
func checkEndpoint(env, raw string) (string, string, error) {
	if raw == "" {
		return "", "", fmt.Errorf("agent: registry: %s is not set", env)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("agent: registry: %s does not parse", env)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "", fmt.Errorf("agent: registry: %s must be an http or https URL", env)
	case u.Host == "":
		return "", "", fmt.Errorf("agent: registry: %s has no host", env)
	case u.User != nil:
		return "", "", fmt.Errorf("agent: registry: %s must not carry credentials", env)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", "", fmt.Errorf("agent: registry: %s must not have a query or fragment", env)
	case u.Path != MCPPath || (u.RawPath != "" && u.RawPath != MCPPath):
		// /mcp-admin hosts the write tools; the agent never goes there.
		return "", "", fmt.Errorf("agent: registry: %s path must be exactly %s", env, MCPPath)
	}
	return u.String(), endpointKey(u), nil
}

// endpointKey is scheme://host:port/path in lower case, with the scheme's
// default port filled in, or "" when u's path is not exactly /mcp (a URL
// with a query or an escaped path gets "" too). bearerTransport adds the
// token only to requests whose key is one of the configured endpoints'.
func endpointKey(u *url.URL) string {
	if u == nil || u.Path != MCPPath || (u.RawPath != "" && u.RawPath != MCPPath) ||
		u.RawQuery != "" || u.ForceQuery || u.User != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(u.Host, "["), "]")
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return ""
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(host), port) + u.Path
}

// quoteTool quotes a server-supplied tool name for an error, cut to
// maxToolText characters.
func quoteTool(name string) string {
	return fmt.Sprintf("%.*q", maxToolText, name)
}

// discoverServer connects to sc, lists its tools and adds them to r,
// within the discover timeout. Any tool without readOnlyHint: true, with
// a name the model providers would refuse, or beyond MaxToolsPerServer
// stops the start.
func (r *Registry) discoverServer(ctx context.Context, sc *serverConn) error {
	ctx, cancel := context.WithTimeout(ctx, r.discover)
	defer cancel()
	cs, err := sc.connect(ctx)
	if err != nil {
		return fmt.Errorf("agent: registry: connect to %s server: %w", sc.name, classify(ctx, err))
	}
	n := 0
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("agent: registry: list %s tools: %w", sc.name, classify(ctx, err))
		}
		if n++; n > MaxToolsPerServer {
			return fmt.Errorf("agent: registry: %s server lists more than %d tools; refusing to start", sc.name, MaxToolsPerServer)
		}
		name := sc.name + toolSep + tool.Name
		if !llmToolName.MatchString(name) || strings.Contains(tool.Name, toolSep) {
			return fmt.Errorf("agent: registry: %s tool %s has a name a model can't call", sc.name, quoteTool(tool.Name))
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			return fmt.Errorf("agent: registry: %s tool %s is not annotated readOnlyHint: true; refusing to start", sc.name, quoteTool(tool.Name))
		}
		if _, dup := r.tools[name]; dup {
			return fmt.Errorf("agent: registry: %s tool %s listed twice", sc.name, quoteTool(tool.Name))
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return fmt.Errorf("agent: registry: %s tool %s input schema does not encode", sc.name, quoteTool(tool.Name))
		}
		r.tools[name] = toolRef{server: sc.name, tool: tool.Name}
		r.specs = append(r.specs, llm.ToolSpec{
			Name:        name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return nil
}

// Specs returns the tools, sorted by name. The slice is a copy.
func (r *Registry) Specs() []llm.ToolSpec {
	return slices.Clone(r.specs)
}

// Execute runs a model's tool call and returns its result for the next
// turn. It never panics and never returns transport detail: an unknown
// name, bad arguments, a tool error, a timeout or a transport failure each
// come back as an IsError result with fixed text. A transport failure is
// retried once on a fresh session. The call is bounded by the registry's
// call timeout (10 s by default).
func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) (res llm.ToolResultBlock) {
	res = llm.ToolResultBlock{ToolCallID: call.ID}
	defer func() {
		if p := recover(); p != nil {
			r.log.ErrorContext(ctx, "agent tool call panicked", "tool", safeName(call.Name))
			res = llm.ToolResultBlock{ToolCallID: call.ID, Content: MsgUnavailable, IsError: true}
		}
	}()
	ref, ok := r.tools[call.Name]
	if !ok {
		r.log.WarnContext(ctx, "agent tool call refused", "tool", safeName(call.Name), "error_class", "unknown_tool")
		res.Content, res.IsError = MsgUnknownTool, true
		return res
	}
	args, ok := decodeArgs(call.Args)
	if !ok {
		r.log.WarnContext(ctx, "agent tool call refused", "tool", call.Name, "error_class", "bad_args")
		res.Content, res.IsError = MsgBadArgs, true
		return res
	}
	out, err := r.call(ctx, ref, args)
	if err != nil {
		res.IsError = true
		switch {
		case errors.Is(err, ErrToolError):
			res.Content = MsgToolError
		case errors.Is(err, ErrTimeout):
			res.Content = MsgTimeout
		default:
			res.Content = MsgUnavailable
		}
		return res
	}
	res.Content = resultText(out)
	return res
}

// decodeArgs reads a tool call's arguments as a JSON object; empty or null
// arguments are an empty object.
func decodeArgs(raw json.RawMessage) (map[string]any, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return map[string]any{}, true
	}
	var args map[string]any
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber() // keep numbers exact; no float on the way through
	if err := dec.Decode(&args); err != nil || args == nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return args, true
}

// safeName is a tool name fit for a log line: unknown names come from the
// model, so only a provider-valid name is logged as given.
func safeName(name string) string {
	if llmToolName.MatchString(name) {
		return name
	}
	return "(invalid name)"
}

// call runs one tool on its server with the registry's timeout. A tool
// error wraps ErrToolError; a deadline wraps ErrTimeout; anything else
// wraps ErrUnavailable after one reconnect and retry. Only the tool name,
// the duration and the error class are logged.
func (r *Registry) call(ctx context.Context, ref toolRef, args any) (*mcp.CallToolResult, error) {
	sc := r.servers[ref.server]
	if sc == nil {
		return nil, ErrUnknownTool
	}
	name := ref.server + toolSep + ref.tool
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	start := time.Now()
	params := &mcp.CallToolParams{Name: ref.tool, Arguments: args}

	used, res, err := sc.callOnce(ctx, params)
	if err != nil && retryable(ctx, err) {
		r.log.WarnContext(ctx, "agent tool call: reconnecting", "tool", name, "error_class", "transport")
		if _, rerr := sc.reconnect(ctx, used); rerr == nil {
			_, res, err = sc.callOnce(ctx, params)
		} else {
			err = rerr
		}
	}
	dur := time.Since(start).Milliseconds()
	if err != nil {
		err = classify(ctx, err)
		r.log.WarnContext(ctx, "agent tool call failed", "tool", name, "duration_ms", dur, "error_class", errClass(err))
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if res.IsError {
		r.log.InfoContext(ctx, "agent tool call returned an error", "tool", name, "duration_ms", dur, "error_class", "tool_error")
		return nil, fmt.Errorf("%s: %w", name, ErrToolError)
	}
	r.log.InfoContext(ctx, "agent tool call", "tool", name, "duration_ms", dur)
	return res, nil
}

// retryable reports whether err is a transport failure worth a fresh
// session: not a deadline or cancellation, and not a JSON-RPC error the
// server answered with (which a retry would only repeat).
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var we *jsonrpc.Error
	if errors.As(err, &we) {
		// The SDK reports a request the transport couldn't deliver as a
		// JSON-RPC error with this code; any other code is the server's
		// answer.
		return we.Code == codeRejectedByTransport
	}
	return true
}

// codeRejectedByTransport is the SDK's code for a request its transport
// failed to send (jsonrpc2.ErrRejected, internal to the SDK).
const codeRejectedByTransport = -32005

// classify maps err to an error class without transport detail.
func classify(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrToolError), errors.Is(err, ErrTimeout), errors.Is(err, ErrUnavailable), errors.Is(err, ErrUnknownTool):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ErrTimeout
	default:
		return ErrUnavailable
	}
}

// errClass names an error class for a log line.
func errClass(err error) string {
	switch {
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrToolError):
		return "tool_error"
	case errors.Is(err, ErrUnknownTool):
		return "unknown_tool"
	default:
		return "transport"
	}
}

// resultText is the text a model sees for a successful call: the result's
// text content joined by newlines (for a structured tool, its JSON).
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Close closes both sessions. It is safe to call more than once.
func (r *Registry) Close() {
	for _, sc := range r.servers {
		sc.close()
	}
}

// ---- one server's session ----

func (sc *serverConn) transport() *mcp.StreamableClientTransport {
	return &mcp.StreamableClientTransport{
		Endpoint:   sc.endpoint,
		HTTPClient: sc.http,
		// The registry reconnects once itself; the SDK doesn't retry.
		MaxRetries: -1,
		// Request and response only; no hanging GET for server pushes.
		DisableStandaloneSSE: true,
	}
}

// connect opens a session and makes it current, closing the one it
// replaces.
func (sc *serverConn) connect(ctx context.Context) (*mcp.ClientSession, error) {
	sc.mu.Lock()
	cs, old, err := sc.connectLocked(ctx)
	sc.mu.Unlock()
	closeSession(old)
	return cs, err
}

// connectLocked is connect with sc.mu held; it returns the replaced
// session for the caller to close once the lock is released. Holding the
// lock across the dial makes reconnects single-flight: concurrent callers
// wait for one new session instead of each opening their own.
func (sc *serverConn) connectLocked(ctx context.Context) (cs, old *mcp.ClientSession, err error) {
	if sc.closed {
		return nil, nil, ErrUnavailable
	}
	cs, err = sc.client.Connect(ctx, sc.transport(), nil)
	if err != nil {
		return nil, nil, err
	}
	old, sc.session = sc.session, cs
	return cs, old, nil
}

func closeSession(cs *mcp.ClientSession) {
	if cs != nil {
		_ = cs.Close()
	}
}

// current returns the current session, or nil.
func (sc *serverConn) current() *mcp.ClientSession {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.session
}

// callOnce calls the tool on the current session and returns the session
// it used, so a failure can be pinned to that session.
func (sc *serverConn) callOnce(ctx context.Context, params *mcp.CallToolParams) (*mcp.ClientSession, *mcp.CallToolResult, error) {
	cs := sc.current()
	if cs == nil {
		return nil, nil, ErrUnavailable
	}
	res, err := cs.CallTool(ctx, params)
	return cs, res, err
}

// reconnect replaces failed with a new session. If another caller has
// already replaced it, the current session is returned as it is, so
// concurrent failures open one session, and nobody closes a session
// another caller is using.
func (sc *serverConn) reconnect(ctx context.Context, failed *mcp.ClientSession) (*mcp.ClientSession, error) {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return nil, ErrUnavailable
	}
	if cur := sc.session; cur != nil && cur != failed {
		sc.mu.Unlock()
		return cur, nil
	}
	// The failed session stays current until its replacement is up, so a
	// failed dial leaves the next call to try again.
	cs, old, err := sc.connectLocked(ctx)
	sc.mu.Unlock()
	closeSession(old)
	return cs, err
}

func (sc *serverConn) close() {
	sc.mu.Lock()
	cs := sc.session
	sc.session = nil
	sc.closed = true
	sc.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
}

// errOffEndpoint is the fixed error for a request outside the two MCP
// endpoints; it names no URL.
var errOffEndpoint = errors.New("agent: request outside the configured MCP endpoints refused")

// bearerTransport adds the agent token to requests to the two configured
// MCP endpoints and refuses every other request, so the token can't
// follow a redirect or reach another allowlisted host. It wraps the httpx
// transport, so the host allowlist still applies.
type bearerTransport struct {
	base    http.RoundTripper
	token   string
	allowed map[string]bool // endpointKey of the books and evidence URLs
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !b.allowed[endpointKey(req.URL)] {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrippers must close the body, even on error
		}
		return nil, errOffEndpoint
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

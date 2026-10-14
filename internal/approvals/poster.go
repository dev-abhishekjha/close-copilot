package approvals

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

// toolPostApproved is books.ToolPostApprovedJournalEntry. approvals must not
// import books (it reaches the ERPNext client), so a test pins the copy.
const toolPostApproved = "post_approved_journal_entry"

// adminPath is mcpkit.AdminPath, pinned by a test.
const adminPath = "/mcp-admin"

// PostTimeout bounds one post: the admin tool's own deadline is 90 s.
const PostTimeout = 100 * time.Second

// RefusalUnknown is the code of a tool refusal whose code isn't one of the
// admin tool's known codes.
const RefusalUnknown = "unknown"

// knownRefusalCodes are the admin tool's refusal codes (books.Code*); a
// test pins the set.
var knownRefusalCodes = map[string]bool{
	"invalid_proposal_id":  true,
	"not_found":            true,
	"invalid_state":        true,
	"not_approved":         true,
	"no_checker":           true,
	"self_approved":        true,
	"not_decided":          true,
	"invalid_posting_date": true,
	"invalid_lines":        true,
	"unbalanced":           true,
	"unknown_company":      true,
	"erp_conflict":         true,
	"erp_mismatch":         true,
	"erp_permission":       true,
	"erp_unavailable":      true,
	"store_unavailable":    true,
}

// PostResult is the admin tool's output.
type PostResult struct {
	ProposalID    string `json:"proposal_id"`
	ERPName       string `json:"erp_name"`
	Status        string `json:"status"`
	AlreadyPosted bool   `json:"already_posted"`
}

// Refusal is the admin tool refusing a post. Code is one of its fixed
// refusal codes, or RefusalUnknown.
type Refusal struct {
	Code string
}

func (r *Refusal) Error() string { return "approvals: post refused: " + r.Code }

// Poster posts an approved proposal to ERPNext.
type Poster interface {
	Post(ctx context.Context, id uuid.UUID) (PostResult, error)
}

// mcpPoster calls post_approved_journal_entry on the books server's
// /mcp-admin route with the admin token. It is the only holder of that
// token; String, GoString and LogValue never show it.
type mcpPoster struct {
	endpoint string
	http     *http.Client
	log      *slog.Logger
	timeout  time.Duration
}

// posterLabel is what a Poster prints as.
const posterLabel = "approvals.Poster(admin MCP)"

func (p *mcpPoster) String() string             { return posterLabel }
func (p *mcpPoster) GoString() string           { return posterLabel }
func (p *mcpPoster) LogValue() slog.Value       { return slog.StringValue(posterLabel) }
func (p *mcpPoster) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(posterLabel)) }

// NewPoster returns the Poster that presents the MCP admin token to the
// books server's /mcp-admin route (cfg.BooksMCPURL with its path replaced).
// It fails closed, with no poster, when the admin token or BooksMCPURL is
// empty, the URL isn't http(s) with a host, or the admin token equals the
// agent token. log, when nil, is slog.Default().
func NewPoster(cfg config.Config, log *slog.Logger) (Poster, error) {
	if log == nil {
		log = slog.Default()
	}
	token := cfg.MCPTokenAdmin.Reveal()
	if token == "" {
		return nil, errors.New("approvals: the MCP admin token is not set")
	}
	if agent := cfg.MCPTokenAgent.Reveal(); agent != "" && subtle.ConstantTimeCompare([]byte(token), []byte(agent)) == 1 {
		return nil, errors.New("approvals: the MCP admin token equals the agent token")
	}
	endpoint, err := adminEndpoint(cfg.BooksMCPURL)
	if err != nil {
		return nil, err
	}
	hc, err := httpx.New(cfg, httpx.WithTimeout(PostTimeout))
	if err != nil {
		return nil, fmt.Errorf("approvals: http client: %w", err)
	}
	eu, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("approvals: admin endpoint is not a URL")
	}
	hc.Transport = &adminTransport{base: hc.Transport, token: token, endpoint: eu}
	// Never follow a redirect: the token must reach only the admin route.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &mcpPoster{endpoint: endpoint, http: hc, log: log, timeout: PostTimeout}, nil
}

// adminEndpoint is booksURL with its path set to /mcp-admin.
func adminEndpoint(booksURL string) (string, error) {
	if strings.TrimSpace(booksURL) == "" {
		return "", errors.New("approvals: BOOKS_MCP_URL is not set")
	}
	u, err := url.Parse(booksURL)
	if err != nil {
		return "", errors.New("approvals: BOOKS_MCP_URL is not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("approvals: BOOKS_MCP_URL must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("approvals: BOOKS_MCP_URL has no host")
	}
	if u.User != nil {
		return "", errors.New("approvals: BOOKS_MCP_URL must not carry credentials")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment, u.ForceQuery = adminPath, "", "", "", false
	return u.String(), nil
}

// adminTransport adds the admin bearer token, and only to requests for the
// admin endpoint; anything else is refused.
type adminTransport struct {
	base     http.RoundTripper
	token    string
	endpoint *url.URL
}

var errOffEndpoint = errors.New("approvals: refused a request off the admin endpoint")

func (t *adminTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameEndpoint(req.URL, t.endpoint) {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrippers must close the body, even on error
		}
		return nil, errOffEndpoint
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// sameEndpoint reports whether u is the admin endpoint: same scheme, host
// and path.
func sameEndpoint(u, endpoint *url.URL) bool {
	return u != nil && strings.EqualFold(u.Scheme, endpoint.Scheme) &&
		strings.EqualFold(u.Host, endpoint.Host) && u.EscapedPath() == endpoint.EscapedPath()
}

// Post calls post_approved_journal_entry with {"proposal_id": id} over a
// short-lived session. A tool error becomes a *Refusal.
func (p *mcpPoster) Post(ctx context.Context, id uuid.UUID) (PostResult, error) {
	if id == uuid.Nil {
		return PostResult{}, errors.New("approvals: post: nil proposal id")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "close-copilot-approvals", Version: buildinfo.Version}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   p.endpoint,
		HTTPClient: p.http,
		// One attempt: the checker retries; the tool is idempotent.
		MaxRetries: -1,
		// Request and response only; no hanging GET for server pushes.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return PostResult{}, fmt.Errorf("approvals: post %s: connect: %w", id, err)
	}
	defer func() {
		if cerr := cs.Close(); cerr != nil {
			p.log.DebugContext(ctx, "approvals: close admin session", "proposal_id", id.String(), "err", cerr)
		}
	}()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolPostApproved,
		Arguments: map[string]string{"proposal_id": id.String()},
	})
	if err != nil {
		return PostResult{}, fmt.Errorf("approvals: post %s: call: %w", id, err)
	}
	return decodeResult(id, res)
}

// decodeResult reads the tool's result: a refusal for an IsError result, a
// checked PostResult otherwise.
func decodeResult(id uuid.UUID, res *mcp.CallToolResult) (PostResult, error) {
	if res == nil {
		return PostResult{}, errors.New("approvals: post: empty result")
	}
	if res.IsError {
		return PostResult{}, &Refusal{Code: refusalCode(resultText(res))}
	}
	var raw []byte
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return PostResult{}, fmt.Errorf("approvals: post %s: encode result: %w", id, err)
		}
		raw = b
	} else {
		raw = []byte(resultText(res))
	}
	var out PostResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return PostResult{}, fmt.Errorf("approvals: post %s: decode result: %w", id, err)
	}
	if out.ProposalID != id.String() || out.Status != StatusPosted || out.ERPName == "" {
		return PostResult{}, fmt.Errorf("approvals: post %s: unexpected result (proposal %q, status %q)", id, out.ProposalID, out.Status)
	}
	return out, nil
}

// refusalCode extracts the code of the tool's fixed "code: message" text,
// RefusalUnknown when it isn't a known code.
func refusalCode(text string) string {
	code, _, ok := strings.Cut(strings.TrimSpace(text), ":")
	if !ok {
		return RefusalUnknown
	}
	code = strings.TrimSpace(code)
	if knownRefusalCodes[code] {
		return code
	}
	return RefusalUnknown
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

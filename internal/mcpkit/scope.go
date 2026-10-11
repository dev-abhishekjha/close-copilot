package mcpkit

// Run scope (CC-506): a run can read only its own company and months, and
// the MCP servers enforce it, not the agent.
//
// Threat model. The agent token (MCP_TOKEN_AGENT) alone reads any company
// and any month. A model steered by the documents it reads, or a tool
// argument injected into a call, could ask for another tenant's books or
// another month. So the agent mints, per run, a short-lived scope signed
// with MCP_SCOPE_KEY and sends it on every /mcp request as X-Run-Scope:
//
//	base64url(json) + "." + base64url(HMAC-SHA256(MCP_SCOPE_KEY, json))
//
// Two lines of defence check it on the server:
//
//  1. The HTTP layer (WithScoper) answers 401 to a request on a scoped path
//     without a valid, unexpired scope, and records the audit actor
//     agent:run/<run_id> from the verified scope, never from the body.
//  2. Every tool handler checks its company and date arguments against the
//     verified scope (Scoper.CheckCompany, Scoper.CheckRange) before any
//     ERPNext or Postgres read, and refuses with an out_of_scope tool
//     error, so the model sees why.
//
// The scope holds no secret and is not encrypted: it says which company
// and which dates a run may read, and the MAC stops anyone without the key
// from changing that. The key never leaves the server processes and the
// agent; the header is never logged and never becomes a span attribute.
//
// Residual risk (accepted): the agent process holds the key, so a
// compromised agent process can mint any scope. This guards against a
// steered model and injected arguments, not a compromised agent.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ScopeHeader carries the run scope on every request to a scoped path.
const ScopeHeader = "X-Run-Scope"

// maxScopeHeader is the longest header Verify decodes; a real scope is
// about 200 bytes.
const maxScopeHeader = 2 << 10

const (
	scopeDate  = "2006-01-02"
	scopeMonth = "2006-01"
)

// ErrOutOfScope is wrapped by every refusal: a call outside the run's
// company or window, or a call without a valid scope. Its text starts
// "out_of_scope:".
var ErrOutOfScope = errors.New("out_of_scope")

// errBadScope is wrapped by every Verify failure. The reasons never quote
// the header.
var errBadScope = errors.New("mcpkit: run scope rejected")

// scopeIDRE bounds a scope's run ID and company ID: they become the audit
// actor and appear in refusal messages.
var scopeIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// Scope is what one run may read: the company ID (as the tools take it,
// "sharma", not the ERPNext name) and an inclusive date window, until Exp
// (unix seconds).
type Scope struct {
	RunID   string `json:"run_id"`
	Company string `json:"company"`
	From    string `json:"from"` // YYYY-MM-DD, inclusive
	To      string `json:"to"`   // YYYY-MM-DD, inclusive
	Exp     int64  `json:"exp"`  // unix seconds; the scope is invalid from then on
}

// scopeError is a refusal the model may read: it names the rule and the
// run's own window, never another company's data.
type scopeError struct{ msg string }

func (e *scopeError) Error() string        { return "out_of_scope: " + e.msg }
func (e *scopeError) Is(target error) bool { return target == ErrOutOfScope }

func outOfScope(format string, a ...any) error {
	return &scopeError{msg: fmt.Sprintf(format, a...)}
}

// Window is the scope of a run of month (YYYY-MM): from the first day of
// the month 12 months earlier, so account history and recurring suppliers
// still work, to the last day of the following month, because the GSTR-2B
// check reads purchase invoices and GSTR-2B entries up to then. 2026-09
// gives 2025-09-01..2026-10-31.
func Window(company, runID, month string, exp time.Time) (Scope, error) {
	m, err := time.Parse(scopeMonth, month)
	if err != nil || m.Format(scopeMonth) != month {
		return Scope{}, fmt.Errorf("mcpkit: run scope: month %.40q is not YYYY-MM", month)
	}
	s := Scope{
		RunID:   runID,
		Company: company,
		From:    m.AddDate(0, -12, 0).Format(scopeDate),
		To:      m.AddDate(0, 2, -1).Format(scopeDate),
		Exp:     exp.Unix(),
	}
	if err := s.validate(); err != nil {
		return Scope{}, err
	}
	return s, nil
}

// DiscoveryScope is the scope of a request made outside any run: connecting
// and listing tools. It passes Verify, but its From is after its To, so
// Allows refuses every read. The company "-discovery-" can never be a real
// company ID.
func DiscoveryScope(now time.Time) Scope {
	return Scope{
		RunID:   "discovery",
		Company: "-discovery-",
		From:    "2000-01-02",
		To:      "2000-01-01",
		Exp:     now.Add(time.Minute).Unix(),
	}
}

// validate checks the fields Verify and Mint require. It does not require
// From <= To: such a scope is valid and reads nothing.
func (s Scope) validate() error {
	if !scopeIDRE.MatchString(s.RunID) {
		return fmt.Errorf("%w: run_id must be 1 to 64 of [A-Za-z0-9._:-]", errBadScope)
	}
	if !scopeIDRE.MatchString(s.Company) {
		return fmt.Errorf("%w: company must be 1 to 64 of [A-Za-z0-9._:-]", errBadScope)
	}
	for _, d := range []string{s.From, s.To} {
		if t, err := time.Parse(scopeDate, d); err != nil || t.Format(scopeDate) != d {
			return fmt.Errorf("%w: from and to must be YYYY-MM-DD dates", errBadScope)
		}
	}
	return nil
}

var scopeEncoding = base64.RawURLEncoding.Strict()

func scopeMAC(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return m.Sum(nil)
}

// Mint signs s with key and returns the X-Run-Scope header value.
func Mint(key []byte, s Scope) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("%w: no scope key", errBadScope)
	}
	if err := s.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("mcpkit: run scope: encode: %w", err)
	}
	return scopeEncoding.EncodeToString(payload) + "." + scopeEncoding.EncodeToString(scopeMAC(key, payload)), nil
}

// Verify checks a header value minted with key and returns its scope. It
// checks the MAC (in constant time) before it decodes anything, then
// requires exactly the Scope fields, a run ID and company, parseable dates
// and Exp after now. Errors never quote the header.
func Verify(key []byte, header string, now time.Time) (Scope, error) {
	switch {
	case len(key) == 0:
		return Scope{}, fmt.Errorf("%w: no scope key", errBadScope)
	case header == "":
		return Scope{}, fmt.Errorf("%w: missing", errBadScope)
	case len(header) > maxScopeHeader:
		return Scope{}, fmt.Errorf("%w: longer than %d bytes", errBadScope, maxScopeHeader)
	}
	p, m, ok := strings.Cut(header, ".")
	if !ok {
		return Scope{}, fmt.Errorf("%w: not payload.mac", errBadScope)
	}
	payload, err := scopeEncoding.DecodeString(p)
	if err != nil {
		return Scope{}, fmt.Errorf("%w: payload is not base64url", errBadScope)
	}
	mac, err := scopeEncoding.DecodeString(m)
	if err != nil {
		return Scope{}, fmt.Errorf("%w: mac is not base64url", errBadScope)
	}
	if !hmac.Equal(mac, scopeMAC(key, payload)) {
		return Scope{}, fmt.Errorf("%w: bad signature", errBadScope)
	}
	var s Scope
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return Scope{}, fmt.Errorf("%w: payload is not a scope", errBadScope)
	}
	if now.Unix() >= s.Exp {
		return Scope{}, fmt.Errorf("%w: expired", errBadScope)
	}
	if err := s.validate(); err != nil {
		return Scope{}, err
	}
	return s, nil
}

// AllowsCompany refuses a company other than the scope's, and every
// company when the scope reads nothing (From after To).
func (s Scope) AllowsCompany(company string) error {
	if s.From > s.To {
		return outOfScope("this session has no run scope; it can list tools but read nothing")
	}
	if company != s.Company {
		return outOfScope("company %.40q is not this run's company; this run may read only %q", company, s.Company)
	}
	return nil
}

// Allows refuses a read of company from..to (YYYY-MM-DD, inclusive) that
// is not inside the scope: another company, a date before From or after
// To, or any read at all when From is after To.
func (s Scope) Allows(company, from, to string) error {
	if err := s.AllowsCompany(company); err != nil {
		return err
	}
	for _, d := range []string{from, to} {
		if t, err := time.Parse(scopeDate, d); err != nil || t.Format(scopeDate) != d {
			return outOfScope("dates must be YYYY-MM-DD inside this run's window %s..%s", s.From, s.To)
		}
	}
	if from < s.From || to > s.To {
		return outOfScope("dates %.40q..%.40q are outside this run's window %s..%s", from, to, s.From, s.To)
	}
	return nil
}

// Month is the run's month: the month before the one To falls in.
func (s Scope) Month() string {
	to, err := time.Parse(scopeDate, s.To)
	if err != nil {
		return ""
	}
	return time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format(scopeMonth)
}

// Scoper verifies the run scope of tool calls on a server. A nil *Scoper
// means unscoped: stdio servers (a local process, no network) and
// in-process tests. Never pass nil from an HTTP code path.
type Scoper struct {
	key []byte
	now func() time.Time
}

// NewScoper returns a Scoper for key (MCP_SCOPE_KEY); an empty key fails.
func NewScoper(key string) (*Scoper, error) {
	if key == "" {
		return nil, errors.New("mcpkit: run scope: MCP_SCOPE_KEY is empty")
	}
	return &Scoper{key: []byte(key), now: time.Now}, nil
}

// verifyHeader verifies the one X-Run-Scope value of h. More than one
// value is refused, so the HTTP layer and the handler can't read
// different ones.
func (s *Scoper) verifyHeader(h http.Header) (Scope, error) {
	if s == nil {
		return Scope{}, fmt.Errorf("%w: no scoper", errBadScope)
	}
	vals := h.Values(ScopeHeader)
	if len(vals) != 1 {
		return Scope{}, fmt.Errorf("%w: want exactly one %s header", errBadScope, ScopeHeader)
	}
	return Verify(s.key, vals[0], s.now())
}

// verifyCall verifies the scope on the HTTP request that carried req.
func (s *Scoper) verifyCall(req *mcp.CallToolRequest) (Scope, error) {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return Scope{}, fmt.Errorf("%w: the call came without HTTP headers", errBadScope)
	}
	return s.verifyHeader(req.Extra.Header)
}

// Scope returns the verified scope of the call, or false when s is nil or
// the call has no valid scope.
func (s *Scoper) Scope(req *mcp.CallToolRequest) (Scope, bool) {
	if s == nil {
		return Scope{}, false
	}
	sc, err := s.verifyCall(req)
	if err != nil {
		return Scope{}, false
	}
	return sc, true
}

// RunID returns the run ID of the call's verified scope.
func (s *Scoper) RunID(req *mcp.CallToolRequest) (string, bool) {
	sc, ok := s.Scope(req)
	return sc.RunID, ok
}

// noScope is the refusal for a call without a valid scope.
func noScope() error {
	return outOfScope("this call carries no valid run scope")
}

// CheckCompany refuses a call whose company is not the scope's. Handlers
// call it before they resolve the company, so another company gets
// out_of_scope rather than "unknown company", which would tell a steered
// model which company IDs exist. A nil s allows everything.
func (s *Scoper) CheckCompany(req *mcp.CallToolRequest, company string) error {
	if s == nil {
		return nil
	}
	sc, err := s.verifyCall(req)
	if err != nil {
		return noScope()
	}
	return sc.AllowsCompany(company)
}

// CheckRange refuses a read of company from..to (YYYY-MM-DD) outside the
// call's scope. A nil s allows everything.
func (s *Scoper) CheckRange(req *mcp.CallToolRequest, company, from, to string) error {
	if s == nil {
		return nil
	}
	sc, err := s.verifyCall(req)
	if err != nil {
		return noScope()
	}
	return sc.Allows(company, from, to)
}

// Check is CheckRange.
func (s *Scoper) Check(req *mcp.CallToolRequest, company, from, to string) error {
	return s.CheckRange(req, company, from, to)
}

// requireScope answers 401 to a request without one valid, unexpired
// X-Run-Scope and otherwise sets the audit actor to agent:run/<run_id>.
// Neither the body nor the log holds the header or the reason.
func requireScope(s *Scoper, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.verifyHeader(r.Header)
		if err != nil {
			http.Error(w, "Unauthorized: missing or invalid run scope", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithActor(r.Context(), "agent:run/"+sc.RunID)))
	})
}

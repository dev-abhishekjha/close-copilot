package agent

// Pseudonymisation of identifiers before a model call (CC-710). GSTINs,
// PANs and the names of individual parties become stable tokens
// (GSTIN_3fa9c1, PAN_08b2de, PERSON_c41e77) before the call, and the
// tokens are restored in what the model writes. A token is a keyed HMAC of
// the run ID, the kind and the value, so the same run rebuilds the same
// tokens on a retry or a resume without stored state.
//
// The LLM is the only third party that sees data (embeddings run on local
// TEI); a GSTIN carries its holder's PAN, which is personal data when the
// supplier is a sole proprietor. Amounts are never touched.
//
// Nothing here logs: values, tokens and names stay out of slog.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/llm"
)

// Identifier patterns. The PAN pass runs after GSTINs are replaced, so the
// PAN inside a GSTIN never becomes a separate token.
var (
	gstinRe = regexp.MustCompile(`\b\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`)
	panRe   = regexp.MustCompile(`\b[A-Z]{5}\d{4}[A-Z]\b`)
	// tokenRe matches token-shaped strings for unmasking.
	tokenRe = regexp.MustCompile(`\b(?:GSTIN|PAN|PERSON)_[0-9a-f]{6,64}\b`)
)

// Token kinds (also the token prefixes).
const (
	kindGSTIN  = "GSTIN"
	kindPAN    = "PAN"
	kindPerson = "PERSON"
)

const (
	tokenHexMin = 6
	tokenHexMax = sha256.Size * 2
)

// Redactor pseudonymises GSTINs, PANs and individual names for one run.
// It is safe for concurrent use; build one per run with NewRedactor.
type Redactor struct {
	key   []byte
	runID uuid.UUID
	names []nameMatcher // longest first

	// sum is the token digest; tests replace it to force collisions.
	sum func(kind, value string) []byte

	mu  sync.Mutex
	fwd map[string]string // kind + "\x00" + value -> token
	rev map[string]string // token -> original value
}

type nameMatcher struct {
	name string
	re   *regexp.Regexp
}

var _ Pseudonymiser = (*Redactor)(nil)

// NewRedactor builds the Redactor of one run. key is the PSEUDONYM_KEY
// bytes; names are the names of individual parties, matched
// case-insensitively as whole words and restored in this casing.
func NewRedactor(key []byte, runID uuid.UUID, names []string) *Redactor {
	r := &Redactor{
		key:   bytes.Clone(key),
		runID: runID,
		fwd:   map[string]string{},
		rev:   map[string]string{},
	}
	r.sum = r.hmac
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		r.names = append(r.names, nameMatcher{name: n, re: regexp.MustCompile(`(?i)` + regexp.QuoteMeta(n))})
	}
	// Longest first, so a name never loses to one it contains.
	slices.SortStableFunc(r.names, func(a, b nameMatcher) int {
		return utf8.RuneCountInString(b.name) - utf8.RuneCountInString(a.name)
	})
	return r
}

// hmac is HMAC-SHA256(key, runID || 0x00 || kind || 0x00 || value).
func (r *Redactor) hmac(kind, value string) []byte {
	m := hmac.New(sha256.New, r.key)
	m.Write(r.runID[:])
	m.Write([]byte{0})
	m.Write([]byte(kind))
	m.Write([]byte{0})
	m.Write([]byte(value))
	return m.Sum(nil)
}

// token returns the value's token, issuing it on first use. A token whose
// first hex digits collide with another value's is lengthened; a token is
// never reused for a different value.
func (r *Redactor) token(kind, value string) string {
	k := kind + "\x00" + value
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.fwd[k]; ok {
		return t
	}
	h := hex.EncodeToString(r.sum(kind, value))
	var t string
	for n := tokenHexMin; ; n += 2 {
		if n > len(h) || n > tokenHexMax {
			// Unreachable with SHA-256 in practice: keep the token unique
			// with a counter.
			t = kind + "_" + h + strconv.Itoa(len(r.rev))
			break
		}
		t = kind + "_" + h[:n]
		if orig, taken := r.rev[t]; !taken || orig == value {
			break
		}
	}
	r.fwd[k] = t
	r.rev[t] = value
	return t
}

// Len is the number of distinct values masked so far (safe to log).
func (r *Redactor) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rev)
}

// MaskString masks GSTINs, then PANs, then individual names in s.
func (r *Redactor) MaskString(s string) string {
	s = gstinRe.ReplaceAllStringFunc(s, func(v string) string { return r.token(kindGSTIN, v) })
	s = panRe.ReplaceAllStringFunc(s, func(v string) string { return r.token(kindPAN, v) })
	for _, n := range r.names {
		s = r.maskName(s, n)
	}
	return s
}

// maskName replaces whole-word, case-insensitive occurrences of one name.
// A word boundary is a letter, digit or underscore on neither side, so a
// name never matches inside a token or a longer word.
func (r *Redactor) maskName(s string, n nameMatcher) string {
	var b strings.Builder
	pos := 0
	for pos < len(s) {
		loc := n.re.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		if !wordBefore(s, start) && !wordAfter(s, end) {
			b.WriteString(s[pos:start])
			b.WriteString(r.token(kindPerson, n.name))
			pos = end
			continue
		}
		// Not a whole word: keep one rune and look again.
		_, size := utf8.DecodeRuneInString(s[start:])
		b.WriteString(s[pos : start+size])
		pos = start + size
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

func isWordRune(c rune) bool { return c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) }

func wordBefore(s string, i int) bool {
	if i == 0 {
		return false
	}
	c, _ := utf8.DecodeLastRuneInString(s[:i])
	return isWordRune(c)
}

func wordAfter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c, _ := utf8.DecodeRuneInString(s[i:])
	return isWordRune(c)
}

// UnmaskString restores every token this Redactor issued; an unknown
// token-shaped string is left unchanged.
func (r *Redactor) UnmaskString(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return tokenRe.ReplaceAllStringFunc(s, func(t string) string {
		if v, ok := r.rev[t]; ok {
			return v
		}
		return t
	})
}

// MaskStrings masks each string of a list (such as ACCOUNTS).
func (r *Redactor) MaskStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.MaskString(s)
	}
	return out
}

// Mask masks every string in a JSON document and returns valid JSON with
// the same structure, key order and numbers. Top-level object keys are
// kept as they are; nested keys and all string values are masked. The
// unmask function restores tokens in text the model wrote.
func (r *Redactor) Mask(data []byte) ([]byte, func([]byte) []byte, error) {
	out, err := rewriteJSON(data, r.MaskString, false)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: redact: %w", err)
	}
	return out, func(b []byte) []byte { return []byte(r.UnmaskString(string(b))) }, nil
}

// unmaskJSON restores tokens in every string of a JSON document (tool-call
// arguments), so restored values are escaped correctly. Bytes that are not
// JSON are unmasked as text.
func (r *Redactor) unmaskJSON(data []byte) []byte {
	if len(bytes.TrimSpace(data)) == 0 {
		return data
	}
	out, err := rewriteJSON(data, r.UnmaskString, true)
	if err != nil {
		return []byte(r.UnmaskString(string(data)))
	}
	return out
}

// rewriteJSON re-encodes one JSON value with f applied to every string,
// keeping key order and exact numbers. Top-level object keys pass through
// f only when topKeys is set.
func rewriteJSON(data []byte, f func(string) string, topKeys bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := rewriteValue(dec, &buf, f, 0, topKeys); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return buf.Bytes(), nil
}

func rewriteValue(dec *json.Decoder, buf *bytes.Buffer, f func(string) string, depth int, topKeys bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			buf.WriteByte('{')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					buf.WriteByte(',')
				}
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if depth > 0 || topKeys {
					key = f(key)
				}
				if err := writeString(buf, key); err != nil {
					return err
				}
				buf.WriteByte(':')
				if err := rewriteValue(dec, buf, f, depth+1, topKeys); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			buf.WriteByte('}')
		case '[':
			buf.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					buf.WriteByte(',')
				}
				if err := rewriteValue(dec, buf, f, depth+1, topKeys); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			buf.WriteByte(']')
		default:
			return fmt.Errorf("unexpected delimiter %q", t)
		}
	case string:
		return writeString(buf, f(t))
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("unexpected JSON token %T", tok)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// ---- the provider decorator ----

// redactingProvider masks a request and unmasks the response.
type redactingProvider struct {
	inner llm.Provider
	r     *Redactor
}

// WrapRedacting returns a provider that masks the text of every system
// block, message, tool result and tool-call argument before calling p,
// and restores the response's text and tool-call arguments. Tool specs,
// the model, amounts and counts are untouched. An error from p is
// returned unchanged, with its (unmasked) response.
func WrapRedacting(p llm.Provider, r *Redactor) llm.Provider {
	return &redactingProvider{inner: p, r: r}
}

func (p *redactingProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if p.inner == nil || p.r == nil {
		return llm.Response{}, errors.New("agent: redacting provider is not configured")
	}
	masked, err := p.maskRequest(req)
	if err != nil {
		return llm.Response{}, err
	}
	resp, callErr := p.inner.Complete(ctx, masked)
	resp.Text = p.r.UnmaskString(resp.Text)
	if len(resp.ToolCalls) > 0 {
		calls := make([]llm.ToolCall, len(resp.ToolCalls))
		for i, c := range resp.ToolCalls {
			c.Args = p.r.unmaskJSON(c.Args)
			calls[i] = c
		}
		resp.ToolCalls = calls
	}
	return resp, callErr
}

func (p *redactingProvider) maskRequest(req llm.Request) (llm.Request, error) {
	out := req
	if req.System != nil {
		out.System = make([]llm.Block, len(req.System))
		for i, b := range req.System {
			b.Text = p.r.MaskString(b.Text)
			out.System[i] = b
		}
	}
	if req.Messages != nil {
		out.Messages = make([]llm.Message, len(req.Messages))
		for i, m := range req.Messages {
			m.Content = p.r.MaskString(m.Content)
			if m.ToolResults != nil {
				results := make([]llm.ToolResultBlock, len(m.ToolResults))
				for j, tr := range m.ToolResults {
					tr.Content = p.r.MaskString(tr.Content)
					results[j] = tr
				}
				m.ToolResults = results
			}
			if m.ToolCalls != nil {
				calls := make([]llm.ToolCall, len(m.ToolCalls))
				for j, c := range m.ToolCalls {
					if len(bytes.TrimSpace(c.Args)) > 0 {
						args, err := rewriteJSON(c.Args, p.r.MaskString, true)
						if err != nil {
							return llm.Request{}, fmt.Errorf("agent: redact tool call %d of message %d: %w", j, i, err)
						}
						c.Args = args
					}
					calls[j] = c
				}
				m.ToolCalls = calls
			}
			out.Messages[i] = m
		}
	}
	return out, nil
}

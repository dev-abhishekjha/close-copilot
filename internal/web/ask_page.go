package web

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// The Ask page (UI-08) is a twin of POST /api/ask: the form post calls the
// same runAsk after the same validation, and POST /ask shares the API's ask
// rate-limit bucket (ratelimit.go). A question and an answer are customer
// data: they are rendered to the asker and nowhere else, never logged, put in
// an error, cookie or header, or stored.

const (
	// maxAskPrefill is the longest ?q= prefill, in bytes.
	maxAskPrefill = 400
	// maxAskEcho bounds the question echoed back after an error. The server
	// body limit applies first; this keeps one page small.
	maxAskEcho = 4000
)

var askSuggestions = []string{
	"Why did bank charges change this month?",
	"Which findings need review before close?",
	"What explains the largest variance this month?",
}

// askRateLimited reports whether r is the form post that shares the ask bucket.
func askRateLimited(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/ask"
}

// cutBytes trims s and cuts it to at most n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s)
}

// askHTML answers 501 with a friendly page until both Options.Ask and
// Options.Runs are set; the role check has run by then.
func (s *Server) askHTML(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ask == nil || s.runs == nil {
			p := principalFrom(r.Context())
			if p == nil {
				unauthorized(w, r)
				return
			}
			s.render(w, r, http.StatusNotImplemented, templates.Ask(templates.AskPage{
				User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf, NotConfigured: true,
			}))
			return
		}
		h(w, r)
	})
}

// renderAsk draws the page. The company options come from the store the API
// validates against; a store error only empties the list.
func (s *Server) renderAsk(w http.ResponseWriter, r *http.Request, status int, p *principal, page templates.AskPage) {
	page.User, page.Role, page.CSRFToken = p.user.Name, string(p.user.Role), p.csrf
	page.Suggestions = askSuggestions
	if s.runs != nil {
		if cs, err := s.runs.b.Store.ListCompanies(r.Context()); err == nil {
			for _, c := range cs {
				page.Companies = append(page.Companies, templates.Option{Value: c.ID, Label: c.ID})
			}
		} else {
			s.log.ErrorContext(r.Context(), "web: ask companies", "path", r.URL.Path, "err", err)
		}
	}
	s.render(w, r, status, templates.Ask(page))
}

// handleAskPage answers GET /ask. ?q= prefills the question, trimmed and cut;
// it is shown in the textarea only.
func (s *Server) handleAskPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	company, month := s.contextFor(r)
	s.renderAsk(w, r, http.StatusOK, p, templates.AskPage{
		Company: company, Month: month, Question: cutBytes(r.URL.Query().Get("q"), maxAskPrefill),
	})
}

// handleAskForm answers POST /ask.
func (s *Server) handleAskForm(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	company, month := r.PostFormValue("company"), r.PostFormValue("month")
	question := r.PostFormValue("question")
	// Only the echo is cut; the backend sees the whole question and refuses a
	// long one itself.
	page := templates.AskPage{Company: company, Month: month, Question: cutBytes(question, maxAskEcho)}
	out, fail := s.runAsk(r.Context(), company, month, question)
	if fail != nil {
		if fail.err != nil {
			// The backend's error never carries the question.
			s.log.ErrorContext(r.Context(), "web: ask", "path", r.URL.Path, "err", fail.err)
		}
		page.Error = fail.msg
		if company == "" && fail.msg == "unknown company" {
			page.Error = "no company selected"
		}
		s.renderAsk(w, r, fail.status, p, page)
		return
	}
	page.Result = askResultView(out)
	s.renderAsk(w, r, http.StatusOK, p, page)
}

func askResultView(out AskOutput) *templates.AskResult {
	r := &templates.AskResult{
		Action: out.Action, Verified: out.Verified, NeedsReview: out.NeedsReview,
		EvidenceIDs: out.EvidenceIDs, StepRefs: out.StepRefs,
	}
	switch out.Action {
	case "answer":
		r.Text = out.Answer
	case "refuse", "clarify":
		r.Text = out.Refusal
	}
	for _, v := range out.Violations {
		row := templates.AskViolationView{Code: v.Code, Field: v.Field, Detail: v.Detail}
		if v.ValuePaise != nil {
			row.Amount = v.ValuePaise.Format()
		}
		r.Violations = append(r.Violations, row)
	}
	for _, c := range out.Citations {
		r.Citations = append(r.Citations, templates.AskCitationView{DocID: c.DocID, Section: c.Section})
	}
	return r
}

// refuseAskPage answers a throttled POST /ask: 429 with Retry-After and the
// Ask page. The form values are echoed only when the CSRF check would pass,
// so a forged post gets nothing of its own back.
func (s *Server) refuseAskPage(w http.ResponseWriter, r *http.Request, p *principal, wait time.Duration) {
	w.Header().Set("Retry-After", retryAfterSeconds(wait))
	company, month := s.contextFor(r)
	page := templates.AskPage{Company: company, Month: month, Error: "You are asking too fast. Try again in a few seconds."}
	if !crossSite(r) && validCSRF(presentedCSRF(r), p.csrf) {
		page.Company, page.Month = r.PostFormValue("company"), r.PostFormValue("month")
		page.Question = cutBytes(r.PostFormValue("question"), maxAskEcho)
	}
	s.renderAsk(w, r, http.StatusTooManyRequests, p, page)
}

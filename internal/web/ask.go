package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Errors an AskBackend returns for the two cases the handler maps to a
// status code; every other error is a 500.
var (
	// ErrQuestionTooLong reports a question over the Asker's length cap (400).
	ErrQuestionTooLong = errors.New("web: question too long")
	// ErrTooManyAsks reports a question routed too many times already (429).
	ErrTooManyAsks = errors.New("web: too many attempts for this question")
)

// AskInput is one validated question. Company is the validated company
// field of the request, never text from Question.
type AskInput struct {
	Company  string
	Month    string
	Question string
}

// AskViolation is one verification failure of an answer.
type AskViolation struct {
	Code       string       `json:"code"`
	Field      string       `json:"field"`
	ValuePaise *money.Paise `json:"value_paise,omitempty"`
	Detail     string       `json:"detail"`
}

// AskOutput is what the handler returns for a routed answer, a refusal or a
// clarification.
type AskOutput struct {
	RunID       uuid.UUID        `json:"run_id"`
	Action      string           `json:"action"`
	Refusal     string           `json:"refusal,omitempty"`
	Answer      string           `json:"answer,omitempty"`
	Citations   []store.Citation `json:"citations"`
	EvidenceIDs []string         `json:"evidence_ids"`
	Verified    bool             `json:"verified"`
	NeedsReview bool             `json:"needs_review"`
	Violations  []AskViolation   `json:"violations,omitempty"`
	StepRefs    []string         `json:"step_refs"`
}

// AskBackend answers a question about one company-month. The wiring adapts
// agent.Asker, so this package imports neither internal/agent nor
// internal/frappe. Any error means "do not answer".
type AskBackend interface {
	Ask(ctx context.Context, in AskInput) (AskOutput, error)
}

// askTimeout bounds one backend call, just under the server's write
// timeout so the client gets the fixed error rather than a cut connection.
const askTimeout = 25 * time.Second

// askError answers 500 with the fixed body. It logs the error under its
// own message; errors from the backend never carry the question.
func (s *Server) askError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "web: ask", "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

type askRequest struct {
	Company  string `json:"company"`
	Month    string `json:"month"`
	Question string `json:"question"`
}

// askAPI answers 501 until both Options.Ask and Options.Runs (whose store
// lists the companies) are set; the role check has run by then.
func (s *Server) askAPI(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ask == nil || s.runs == nil {
			notImplemented.ServeHTTP(w, r)
			return
		}
		h(w, r)
	})
}

// askFailure is a refused or failed ask: the status and the fixed message
// both the JSON API and the form page show. err is set only for a 500 and is
// logged by the caller; it never carries the question.
type askFailure struct {
	status int
	msg    string
	err    error
}

// runAsk is the one path to the model for POST /api/ask and POST /ask: the
// same validation, the same company check, the same backend call and the same
// error mapping. question is trimmed here.
func (s *Server) runAsk(ctx context.Context, company, month, question string) (AskOutput, *askFailure) {
	if !monthRE.MatchString(month) {
		return AskOutput{}, &askFailure{status: http.StatusBadRequest, msg: "month must be YYYY-MM"}
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return AskOutput{}, &askFailure{status: http.StatusBadRequest, msg: "question must not be empty"}
	}
	companies, err := s.runs.b.Store.ListCompanies(ctx)
	if err != nil {
		return AskOutput{}, &askFailure{status: http.StatusInternalServerError, msg: "internal error", err: err}
	}
	if !slices.ContainsFunc(companies, func(c store.Company) bool { return c.ID == company }) {
		return AskOutput{}, &askFailure{status: http.StatusBadRequest, msg: "unknown company"}
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	out, err := s.ask.Ask(ctx, AskInput{Company: company, Month: month, Question: question})
	switch {
	case err == nil:
	case errors.Is(err, ErrQuestionTooLong):
		return AskOutput{}, &askFailure{status: http.StatusBadRequest, msg: "question is too long"}
	case errors.Is(err, ErrTooManyAsks):
		return AskOutput{}, &askFailure{status: http.StatusTooManyRequests, msg: "too many attempts for this question"}
	default:
		// A deadline or cancellation also lands here: nothing is answered.
		return AskOutput{}, &askFailure{status: http.StatusInternalServerError, msg: "internal error", err: err}
	}
	if out.Citations == nil {
		out.Citations = []store.Citation{}
	}
	if out.EvidenceIDs == nil {
		out.EvidenceIDs = []string{}
	}
	if out.StepRefs == nil {
		out.StepRefs = []string{}
	}
	return out, nil
}

// handleAPIAsk answers POST /api/ask.
//
// Until per-user company lists exist (an owner decision before CC-1201),
// any signed-in user can ask about any company the store lists.
func (s *Server) handleAPIAsk(w http.ResponseWriter, r *http.Request) {
	var in askRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be one JSON object with company, month and question"})
		return
	}
	out, fail := s.runAsk(r.Context(), in.Company, in.Month, in.Question)
	if fail != nil {
		if fail.err != nil {
			s.askError(w, r, fail.err)
			return
		}
		writeJSON(w, fail.status, map[string]string{"error": fail.msg})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

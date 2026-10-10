package agent

// The recording provider (CC-709). RecordingProvider wraps an llm.Provider
// and keeps what was said: per Complete it stores the full llm.Request as a
// prompt artifact, the llm.Response as a response artifact, and one
// llm_calls row with model, tokens, cost and latency. Prompts and responses
// are stored verbatim; pseudonymisation (CC-710) wraps this provider from
// the outside, so the stored prompt is exactly what the model saw.
//
// The wrapper never repeats or retries the model call, and never logs
// prompt or response content.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// LLMCallRecorder stores artifacts and llm_calls rows. *store.Store
// implements it.
type LLMCallRecorder interface {
	ArtifactPutter
	InsertLLMCall(ctx context.Context, c store.LLMCall) (uuid.UUID, error)
}

var _ LLMCallRecorder = (*store.Store)(nil)

// DefaultRecordTimeout bounds the writes that record a call after the
// model has answered. They run detached from the caller's cancellation, so
// a call already paid for is still recorded, but never for longer than
// this.
const DefaultRecordTimeout = 10 * time.Second

// RecordingProvider is an llm.Provider that records every call of the
// provider it wraps against one run step.
type RecordingProvider struct {
	inner         llm.Provider
	rec           LLMCallRecorder
	runID         uuid.UUID
	stepID        uuid.UUID
	now           func() time.Time
	recordTimeout time.Duration
}

var _ llm.Provider = (*RecordingProvider)(nil)

// NewRecordingProvider wraps inner so its calls are recorded as artifacts
// of runID and llm_calls rows of stepID.
func NewRecordingProvider(inner llm.Provider, rec LLMCallRecorder, runID, stepID uuid.UUID) *RecordingProvider {
	return &RecordingProvider{inner: inner, rec: rec, runID: runID, stepID: stepID, now: time.Now, recordTimeout: DefaultRecordTimeout}
}

// Complete stores the prompt, calls the wrapped provider once, then stores
// the response and an llm_calls row. A provider error that still carries a
// usable Response (such as llm.ErrUnpricedModel) is recorded and then
// returned; one with no response is returned without a record. If
// recording fails after the call, the response is returned together with
// the recording error, and the call is not repeated.
func (p *RecordingProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if p.inner == nil || p.rec == nil {
		return llm.Response{}, errors.New("agent: recording provider is not configured")
	}
	if p.stepID == uuid.Nil || p.runID == uuid.Nil {
		return llm.Response{}, errors.New("agent: recording provider has no run or step")
	}
	// Store the prompt before spending anything on the call: an
	// unrecordable prompt never reaches the model.
	promptSHA, err := p.rec.PutArtifact(ctx, store.ArtifactPrompt, p.runID, p.stepID, req)
	if err != nil {
		return llm.Response{}, fmt.Errorf("agent: record prompt: %w", err)
	}

	start := p.now()
	resp, callErr := p.inner.Complete(ctx, req)
	latency := p.now().Sub(start).Milliseconds()

	if callErr != nil && !usableResponse(resp) {
		return resp, callErr
	}

	// The call has happened and may have cost money: record it even if
	// the caller's context is now cancelled, within a bounded time.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.recordTimeout)
	defer cancel()
	if recErr := p.record(rctx, req, resp, promptSHA, latency); recErr != nil {
		return resp, errors.Join(callErr, recErr)
	}
	return resp, callErr
}

func (p *RecordingProvider) record(ctx context.Context, req llm.Request, resp llm.Response, promptSHA string, latencyMS int64) error {
	respSHA, err := p.rec.PutArtifact(ctx, store.ArtifactResponse, p.runID, p.stepID, resp)
	if err != nil {
		return fmt.Errorf("agent: record response: %w", err)
	}
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	u, err := usageOf(resp.Usage)
	if err != nil {
		return fmt.Errorf("agent: record llm call: %w", err)
	}
	if _, err := p.rec.InsertLLMCall(ctx, store.LLMCall{
		RunID:           p.runID,
		StepID:          p.stepID,
		Model:           model,
		PromptSHA256:    promptSHA,
		ResponseSHA256:  respSHA,
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
		CostUSD:         string(u.CostUSD),
		LatencyMS:       latencyMS,
	}); err != nil {
		return fmt.Errorf("agent: record llm call: %w", err)
	}
	return nil
}

// usableResponse reports whether a provider error came with a response
// worth recording: a resolved model, tokens or output.
func usableResponse(r llm.Response) bool {
	return r.Model != "" || r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0 ||
		r.Text != "" || len(r.ToolCalls) > 0
}

// usageWire is llm.Usage as JSON, with the cost as exact decimal text.
type usageWire struct {
	InputTokens     int64       `json:"input_tokens"`
	OutputTokens    int64       `json:"output_tokens"`
	CacheReadTokens int64       `json:"cache_read_tokens"`
	CostUSD         json.Number `json:"cost_usd"`
}

// usageOf converts usage through its JSON form, so the float cost field is
// never selected here (nofloat) and reaches the store as decimal text.
func usageOf(u llm.Usage) (usageWire, error) {
	b, err := json.Marshal(u)
	if err != nil {
		return usageWire{}, fmt.Errorf("encode usage: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var w usageWire
	if err := dec.Decode(&w); err != nil {
		return usageWire{}, fmt.Errorf("decode usage: %w", err)
	}
	return w, nil
}

package llm

import (
	"context"
	"sync"
)

// ScriptedCall defines a canned response or error to be returned by FakeProvider.
type ScriptedCall struct {
	Response Response
	Err      error
}

// FakeProvider is a thread-safe Provider implementation for unit testing that
// replays scripted responses and records all incoming requests.
type FakeProvider struct {
	mu       sync.Mutex
	calls    []Request
	script   []ScriptedCall
	fallback *Response
}

// NewFakeProvider returns a FakeProvider initialized with the given scripted responses.
func NewFakeProvider(script ...ScriptedCall) *FakeProvider {
	return &FakeProvider{
		script: append([]ScriptedCall(nil), script...),
	}
}

// WithFallback sets a fallback response to return when the script is exhausted.
func (f *FakeProvider) WithFallback(resp Response) *FakeProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallback = &resp
	return f
}

// Complete records the request and returns the next scripted response.
func (f *FakeProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, req)

	if len(f.script) > 0 {
		next := f.script[0]
		f.script = f.script[1:]
		return next.Response, next.Err
	}

	if f.fallback != nil {
		return *f.fallback, nil
	}

	return Response{
		Text:       "fake response",
		StopReason: "end_turn",
		Usage: Usage{
			InputTokens:  10,
			OutputTokens: 10,
		},
	}, nil
}

// Calls returns a copy of all requests received by the provider so far.
func (f *FakeProvider) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := make([]Request, len(f.calls))
	copy(copied, f.calls)
	return copied
}

// CallCount returns the total number of calls received.
func (f *FakeProvider) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// LastCall returns the most recent request received, or false if no calls were made.
func (f *FakeProvider) LastCall() (Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.calls) == 0 {
		return Request{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// Reset clears recorded calls and remaining scripted items.
func (f *FakeProvider) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.script = nil
}

package agent

// Fault injection (CC-705). COPILOT_FAULT=corrupt_explanation makes the
// verifier's retry loop visible end to end (Gate B): on attempt 1 of
// exactly one explain step per process, a fixed sentence with an invented
// amount is appended to the stored explanation, after the explainer's own
// validation and before the artifact is written. The verifier then fails
// it with amount_not_in_evidence, and the explainer retries with the
// violations.
//
// Hard limits: it only appends text to the explanation, so it can only add
// an amount for the verifier to reject, never remove one; it never touches
// the suggested action, cited amounts, citations, the proposal, the
// accounts or anything written to ERPNext. The artifact is marked
// fault_injected, the explainer logs a warning, and the run report says so
// when a stored explanation of the run carries the mark (read from the
// artifacts, not from this process's setting). Only cmd/agent turns it on;
// the zero value is off.

import (
	"fmt"
	"sync/atomic"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// FaultSentence is what corrupt_explanation appends. ₹9,87,654.32 is
// invented: no synthetic evidence holds it.
const FaultSentence = " The total exposure is ₹9,87,654.32."

// Fault is the fault injector. A nil *Fault is off.
type Fault struct {
	mode string
	used atomic.Bool
}

// NewFault returns the injector for a COPILOT_FAULT value: nil for "",
// one for config.FaultCorruptExplanation, and an error for anything else.
func NewFault(mode string) (*Fault, error) {
	switch mode {
	case "":
		return nil, nil
	case config.FaultCorruptExplanation:
		return &Fault{mode: mode}, nil
	}
	return nil, fmt.Errorf("agent: %s must be empty or %q, got %.40q", config.EnvCopilotFault, config.FaultCorruptExplanation, mode)
}

// Mode is the fault's COPILOT_FAULT value ("" when off).
func (f *Fault) Mode() string {
	if f == nil {
		return ""
	}
	return f.mode
}

// corruptExplanation appends FaultSentence to the explanation of the first
// attempt-1 explain step that reaches it, once per Fault, and marks the
// artifact. It reports whether it did.
func (f *Fault) corruptExplanation(attempt int, art *ExplanationArtifact) bool {
	if f == nil || f.mode != config.FaultCorruptExplanation || attempt != 1 || art == nil {
		return false
	}
	if !f.used.CompareAndSwap(false, true) {
		return false
	}
	art.Explanation += FaultSentence
	art.FaultInjected = true
	return true
}

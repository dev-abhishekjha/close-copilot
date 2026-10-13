// Package prompts holds the agent's system prompts, embedded at build time
// so a prompt change is a code change with a diff and a commit.
package prompts

import (
	_ "embed" // for go:embed
	"strings"
)

// Explain is the explainer's static system prompt (CC-704). The account
// list is appended at run time, in the same cached block.
//
//go:embed explain.txt
var Explain string

// Investigate is the exceptions investigator's static system prompt
// (CC-706). The account list is appended at run time, in the same cached
// block.
//
//go:embed investigate.txt
var Investigate string

// Triage is the question router's static system prompt (CC-708). The
// router's user message holds only the company id and the fenced question.
//
//go:embed triage.txt
var Triage string

//go:embed ask.txt
var askFile string

// askGatherMarker separates the two prompts in ask.txt.
const askGatherMarker = "=== GATHER ===\n"

// Ask is the static system prompt of the Asker's answer call (CC-707),
// whose only tool is emit_answer. AskGather is the static system prompt of
// its number-gathering loop, which sees read tools and never documents.
// Both live in ask.txt, split at the line "=== GATHER ===".
var Ask, AskGather = splitAsk(askFile)

func splitAsk(s string) (string, string) {
	answer, gather, ok := strings.Cut(s, askGatherMarker)
	if !ok || strings.TrimSpace(answer) == "" || strings.TrimSpace(gather) == "" {
		panic("prompts: ask.txt needs an answer prompt, the line " + strings.TrimSpace(askGatherMarker) + " and a gather prompt")
	}
	return answer, gather
}

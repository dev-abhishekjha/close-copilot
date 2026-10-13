// Package prompts holds the agent's system prompts, embedded at build time
// so a prompt change is a code change with a diff and a commit.
package prompts

import _ "embed" // for go:embed

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

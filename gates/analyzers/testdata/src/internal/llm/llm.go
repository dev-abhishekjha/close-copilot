// Package llm is outside nofloat's scope: cost is tracked in dollars.
package llm

var costPerMTok float32 = 3.0

func cost(tokens int) float64 { return float64(tokens) * float64(costPerMTok) / 1e6 }

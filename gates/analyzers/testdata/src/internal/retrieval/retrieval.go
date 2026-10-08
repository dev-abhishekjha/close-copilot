// Package retrieval is outside nofloat's scope: scores are floats.
package retrieval

func rrf(rank int) float64 { return 1.0 / (60.0 + float64(rank)) }

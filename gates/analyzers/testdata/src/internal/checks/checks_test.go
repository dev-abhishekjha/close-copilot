package checks

// Test files are exempt from nofloat.
var tolerance = 0.01

func ratio(a, b int64) float64 { return float64(a) / float64(b) }

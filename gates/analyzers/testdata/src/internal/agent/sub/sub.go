package sub

import "strconv"

func score(n int) float64 { // want `float type float64 in internal/agent/sub`
	return float64(n) / 2 // want `float type float64 in internal/agent/sub`
}

func parse(s string) error {
	v, err := strconv.ParseFloat(s, 64) // want `float type float64 in internal/agent/sub`
	_ = v                               // want `float type float64 in internal/agent/sub`
	return err
}

func ratio() float32 { return 0 } // want `float type float32 in internal/agent/sub`

type Rate float64 // want `float type float64 in internal/agent/sub`

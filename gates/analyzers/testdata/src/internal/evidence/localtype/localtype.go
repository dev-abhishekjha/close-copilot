// Package localtype shadows float64 with an integer type: nofloat uses
// type information, so this is clean.
package localtype

type float64 int64

var amount float64 = 3

func double(x float64) float64 { return x * 2 }

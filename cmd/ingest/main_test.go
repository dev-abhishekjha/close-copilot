package main

import "testing"

func TestStopArgsNameOnlyDocling(t *testing.T) {
	n := len(stopArgs)
	if n < 2 || stopArgs[n-2] != "stop" || stopArgs[n-1] != "docling" {
		t.Fatalf("stopArgs must end with `stop docling`, got %v", stopArgs)
	}
}

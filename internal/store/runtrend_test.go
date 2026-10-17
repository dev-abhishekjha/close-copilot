package store

import "testing"

func TestRecentRunTrendLimit(t *testing.T) {
	tests := []struct{ in, want int }{
		{-5, 1}, {0, 1}, {1, 1}, {12, 12}, {50, 50}, {51, 50}, {10000, 50},
	}
	for _, tt := range tests {
		if got := clampRunTrendLimit(tt.in); got != tt.want {
			t.Errorf("clampRunTrendLimit(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

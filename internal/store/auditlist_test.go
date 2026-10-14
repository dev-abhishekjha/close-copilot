package store

import "testing"

func TestAuditFilterLimit(t *testing.T) {
	tests := []struct{ in, want int }{
		{-5, 50}, {0, 50}, {1, 1}, {50, 50}, {200, 200}, {201, 200}, {100000, 200},
	}
	for _, tt := range tests {
		if got := auditLimit(tt.in); got != tt.want {
			t.Errorf("auditLimit(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

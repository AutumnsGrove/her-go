package reply

import (
	"strings"
	"testing"
)

func TestLengthDirectiveFor(t *testing.T) {
	tests := []struct {
		length  string
		wantSub string
	}{
		{"brief", "SHORT"},
		{"normal", "1-3 sentences"},
		{"detailed", "longer, more thoughtful"},
		{"", "SHORT"},
		{"unknown_value", "SHORT"},
	}

	for _, tt := range tests {
		t.Run(tt.length, func(t *testing.T) {
			got := lengthDirectiveFor(tt.length)
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("lengthDirectiveFor(%q) = %q, want substring %q", tt.length, got, tt.wantSub)
			}
			if !strings.HasPrefix(got, "Length:") {
				t.Errorf("lengthDirectiveFor(%q) should start with 'Length:', got %q", tt.length, got)
			}
		})
	}
}

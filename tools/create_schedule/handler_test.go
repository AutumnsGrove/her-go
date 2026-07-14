package create_schedule

import (
	"encoding/json"
	"testing"
)

func TestUnwrapStringifiedJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "proper object passes through unchanged",
			input: `{"message":"hi"}`,
			want:  `{"message":"hi"}`,
		},
		{
			name:  "double-encoded string is unwrapped",
			input: `"{\"message\":\"hi\"}"`,
			want:  `{"message":"hi"}`,
		},
		{
			name:  "plain string that isn't JSON stays unchanged",
			input: `"just a string"`,
			want:  `"just a string"`,
		},
		{
			name:  "empty object passes through",
			input: `{}`,
			want:  `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapStringifiedJSON(json.RawMessage(tc.input))
			if string(got) != tc.want {
				t.Errorf("unwrapStringifiedJSON(%s) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

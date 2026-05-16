package reply

import "testing"

func TestReduceEmDashes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no em dashes",
			in:   "Hello world. This is fine.",
			want: "Hello world. This is fine.",
		},
		{
			name: "single em dash preserved",
			in:   "I love coffee — especially espresso.",
			want: "I love coffee — especially espresso.",
		},
		{
			name: "two em dashes preserved",
			in:   "She arrived — finally — at noon.",
			want: "She arrived — finally — at noon.",
		},
		{
			name: "three em dashes triggers replacement",
			in:   "First — Second — Third — Fourth.",
			want: "First. Second. Third. Fourth.",
		},
		{
			name: "capital after dash becomes period",
			in:   "One thing — Another thing — Yet another — Done.",
			want: "One thing. Another thing. Yet another. Done.",
		},
		{
			name: "lowercase after dash becomes comma",
			in:   "She was tired — exhausted really — from the work — which never ended.",
			want: "She was tired, exhausted really, from the work, which never ended.",
		},
		{
			name: "mixed case",
			in:   "I tried — but failed — She tried — and succeeded.",
			want: "I tried, but failed. She tried, and succeeded.",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reduceEmDashes(tt.in)
			if got != tt.want {
				t.Errorf("reduceEmDashes(%q)\n  got:  %q\n  want: %q", tt.in, got, tt.want)
			}
		})
	}
}

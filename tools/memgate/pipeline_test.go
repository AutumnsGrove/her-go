package memgate

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Gate 1: Style blocklist
// ---------------------------------------------------------------------------

func TestStyleGate_TrailingEmDash(t *testing.T) {
	input := PipelineInput{Text: "She likes coffee —", Subject: "user"}
	v := RunPipeline(input, PipelineDeps{})

	if v.Allowed {
		t.Error("trailing em dash should be rejected")
	}
	if !strings.Contains(v.Reason, "trailing em dash") {
		t.Errorf("expected em dash rejection, got: %s", v.Reason)
	}
}

func TestStyleGate_TrailingEnDash(t *testing.T) {
	input := PipelineInput{Text: "She likes coffee –", Subject: "user"}
	v := RunPipeline(input, PipelineDeps{})

	if v.Allowed {
		t.Error("trailing en dash should be rejected")
	}
}

func TestStyleGate_MidSentenceEmDashAllowed(t *testing.T) {
	input := PipelineInput{Text: "She likes coffee — especially lattes.", Subject: "user"}
	v := RunPipeline(input, PipelineDeps{})

	if !v.Allowed {
		t.Errorf("mid-sentence em dash should be allowed, got: %s", v.Reason)
	}
}

func TestStyleGate_BlockedPattern(t *testing.T) {
	tests := []struct {
		text    string
		blocked bool
	}{
		{"This is a significant moment in her career", true},
		{"She delves into machine learning", true},
		{"A testament to her hard work", true},
		{"She likes coffee and coding", false},
		{"She started a new project today", false},
	}

	for _, tt := range tests {
		v := RunPipeline(PipelineInput{Text: tt.text, Subject: "user"}, PipelineDeps{})
		if tt.blocked && v.Allowed {
			t.Errorf("expected %q to be blocked", tt.text)
		}
		if !tt.blocked && !v.Allowed {
			t.Errorf("expected %q to be allowed, got: %s", tt.text, v.Reason)
		}
	}
}

// ---------------------------------------------------------------------------
// Gate 2: Length
// ---------------------------------------------------------------------------

func TestLengthGate_DefaultLimit(t *testing.T) {
	long := strings.Repeat("x", 1001)
	v := RunPipeline(PipelineInput{Text: long, Subject: "user"}, PipelineDeps{})

	if v.Allowed {
		t.Error("memory over 1000 chars should be rejected")
	}
	if !strings.Contains(v.Reason, "1001 characters") {
		t.Errorf("expected length in rejection, got: %s", v.Reason)
	}
}

func TestLengthGate_CustomLimit(t *testing.T) {
	text := strings.Repeat("x", 501)
	v := RunPipeline(
		PipelineInput{Text: text, Subject: "user"},
		PipelineDeps{MaxLength: 500},
	)

	if v.Allowed {
		t.Error("memory over custom limit should be rejected")
	}
}

func TestLengthGate_ExactLimit(t *testing.T) {
	text := strings.Repeat("x", 1000)
	v := RunPipeline(PipelineInput{Text: text, Subject: "user"}, PipelineDeps{})

	if !v.Allowed {
		t.Errorf("memory at exactly 1000 chars should be allowed, got: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Happy path (no classifier, no dedup)
// ---------------------------------------------------------------------------

func TestPipeline_HappyPath(t *testing.T) {
	v := RunPipeline(
		PipelineInput{
			Text:    "She enjoys hiking in Forest Park on weekends.",
			Subject: "user",
			Tags:    "hobbies, outdoors",
		},
		PipelineDeps{},
	)

	if !v.Allowed {
		t.Errorf("simple clean memory should pass, got: %s", v.Reason)
	}
}

func TestPipeline_SelfMemory(t *testing.T) {
	// Self-memories follow the same style/length gates.
	v := RunPipeline(
		PipelineInput{
			Text:    "I noticed I tend to over-explain technical concepts.",
			Subject: "self",
		},
		PipelineDeps{},
	)

	if !v.Allowed {
		t.Errorf("clean self-memory should pass, got: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Pre-approved bypass
// ---------------------------------------------------------------------------

func TestPipeline_PreApprovedBypass(t *testing.T) {
	preApproved := map[string]bool{
		"she likes coffee": true,
	}
	v := RunPipeline(
		PipelineInput{Text: "She likes coffee", Subject: "user"},
		PipelineDeps{
			// ClassifierLLM is nil, so classifier gate is skipped anyway.
			// But we verify the pre-approved map is checked.
			PreApproved: preApproved,
		},
	)

	if !v.Allowed {
		t.Errorf("pre-approved memory should pass, got: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Skip dedup flag
// ---------------------------------------------------------------------------

func TestPipeline_SkipDedupFlag(t *testing.T) {
	// With SkipDedup=true and no classifier, everything passes.
	v := RunPipeline(
		PipelineInput{Text: "She moved to Portland.", Subject: "user"},
		PipelineDeps{SkipDedup: true},
	)

	if !v.Allowed {
		t.Errorf("should pass with dedup skipped, got: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Update memory (OldText shows delta to classifier)
// ---------------------------------------------------------------------------

func TestPipeline_UpdateWithOldText(t *testing.T) {
	// No classifier LLM, so this just verifies OldText doesn't break anything.
	v := RunPipeline(
		PipelineInput{
			Text:    "She moved to Portland in 2024.",
			Subject: "user",
			OldText: "She moved to Portland.",
		},
		PipelineDeps{SkipDedup: true},
	)

	if !v.Allowed {
		t.Errorf("update with old text should pass, got: %s", v.Reason)
	}
}

// ---------------------------------------------------------------------------
// Exported helpers
// ---------------------------------------------------------------------------

func TestStyleBlocklist_Exported(t *testing.T) {
	bl := StyleBlocklist()
	if len(bl) == 0 {
		t.Error("StyleBlocklist() should not be empty")
	}
}

func TestMaxLength_Exported(t *testing.T) {
	if MaxLength() != 1000 {
		t.Errorf("MaxLength() = %d, want 1000", MaxLength())
	}
}

package compact

import (
	"os"
	"testing"

	"her/memory"
)

// makeMessages creates N fake messages alternating user/assistant,
// each with roughly charsPerMsg characters of content. This lets us
// control the token estimate precisely:
//   tokens per message = len(content)/4 + 10 (overhead)
func makeMessages(n int, charsPerMsg int) []memory.Message {
	msgs := make([]memory.Message, n)
	content := make([]byte, charsPerMsg)
	for i := range content {
		content[i] = 'a' // fill with 'a's — content doesn't matter for token counting
	}
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = memory.Message{
			ID:              int64(i + 1),
			Role:            role,
			ContentRaw:      string(content),
			ContentScrubbed: string(content),
		}
	}
	return msgs
}

func TestEstimateTokens(t *testing.T) {
	// 4 chars = 1 token, so 100 chars = 25 tokens
	if got := estimateTokens("aaaa"); got != 1 {
		t.Errorf("estimateTokens(4 chars) = %d, want 1", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Errorf("estimateTokens(empty) = %d, want 0", got)
	}
}

func TestEstimateHistoryTokens(t *testing.T) {
	// Each message: 100 chars = 25 tokens + 10 overhead = 35 tokens per message
	msgs := makeMessages(10, 100)
	got := EstimateHistoryTokens("", msgs)
	want := 10 * (100/4 + 10) // 10 * 35 = 350
	if got != want {
		t.Errorf("EstimateHistoryTokens(10 msgs, 100 chars each) = %d, want %d", got, want)
	}

	// With a summary string
	summary := string(make([]byte, 200)) // 200 chars = 50 tokens
	got = EstimateHistoryTokens(summary, msgs)
	want = 50 + 350 // summary + messages
	if got != want {
		t.Errorf("EstimateHistoryTokens(with summary) = %d, want %d", got, want)
	}
}

func TestEstimateHistoryTokens_RealCounts(t *testing.T) {
	// Assistant messages with real TokenCount should use that instead of
	// estimating from content length.
	msgs := makeMessages(4, 100)
	// Set real token counts on the assistant messages (odd indices).
	// 100 chars would estimate to 25 tokens, but real count is 42.
	msgs[1].TokenCount = 42
	msgs[3].TokenCount = 55

	got := EstimateHistoryTokens("", msgs)
	// User messages (0, 2): estimated = 25 + 10 = 35 each → 70
	// Assistant messages (1, 3): real counts = 42+10 + 55+10 = 117
	want := 70 + 117
	if got != want {
		t.Errorf("EstimateHistoryTokens(real counts) = %d, want %d", got, want)
	}

	// User messages with TokenCount should still estimate (it stores
	// total prompt tokens, not per-message size).
	msgs[0].TokenCount = 9999 // should be ignored
	got2 := EstimateHistoryTokens("", msgs)
	if got2 != want {
		t.Errorf("user TokenCount should be ignored: got %d, want %d", got2, want)
	}
}

func TestMaybeCompact_UnderThreshold(t *testing.T) {
	// Create a temp store so MaybeCompact can call LatestSummary.
	tmpFile, err := os.CreateTemp("", "compact-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := memory.NewStore(tmpFile.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 10 messages, 100 chars each = 350 estimated tokens.
	// With maxHistoryTokens=1400, threshold = 1050. 350 < 1050 → no compaction.
	msgs := makeMessages(10, 100)
	cr, err := MaybeCompact(nil, store, "test-conv", msgs, 1400, "Mira", "User")
	if err != nil {
		t.Fatal(err)
	}
	if cr.DidCompact {
		t.Error("expected no compaction (under threshold), but DidCompact=true")
	}
	if len(cr.KeptMessages) != 10 {
		t.Errorf("expected all 10 messages kept, got %d", len(cr.KeptMessages))
	}
}

func TestMaybeCompact_OverThreshold(t *testing.T) {
	// Create a temp store.
	tmpFile, err := os.CreateTemp("", "compact-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := memory.NewStore(tmpFile.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 40 messages, 100 chars each = 40 * 35 = 1400 estimated tokens.
	// With maxHistoryTokens=1400, threshold = 1050. 1400 > 1050 → should compact.
	// But chatLLM is nil, so the summarization call will fail and it'll
	// return without compacting — that's fine, the key test is whether
	// the threshold check PASSES (i.e., it doesn't return early).
	msgs := makeMessages(40, 100)

	// We can't test actual compaction without an LLM, but we CAN verify
	// the token math by checking EstimateHistoryTokens directly.
	tokens := EstimateHistoryTokens("", msgs)
	threshold := int(float64(1400) * 0.75) // 1050
	if tokens < threshold {
		t.Errorf("expected tokens (%d) >= threshold (%d), but it's under", tokens, threshold)
	}
	t.Logf("40 messages, 100 chars each: %d estimated tokens (threshold: %d)", tokens, threshold)

	// Now test with a smaller message set that should be UNDER threshold.
	smallMsgs := makeMessages(20, 100)
	smallTokens := EstimateHistoryTokens("", smallMsgs)
	if smallTokens >= threshold {
		t.Errorf("expected small set tokens (%d) < threshold (%d)", smallTokens, threshold)
	}
	t.Logf("20 messages, 100 chars each: %d estimated tokens (threshold: %d)", smallTokens, threshold)
}

func TestMaybeCompact_ZeroBudget_UsesDefault(t *testing.T) {
	// When maxHistoryTokens is 0 (not set in config), it should use
	// the default of 8000. This is the case that was broken in production.
	tmpFile, err := os.CreateTemp("", "compact-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := memory.NewStore(tmpFile.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Pass maxHistoryTokens=0 (simulating unset config).
	// 10 messages, 100 chars each = 350 tokens. Should be under default threshold (6000).
	msgs := makeMessages(10, 100)
	cr, err := MaybeCompact(nil, store, "test-conv", msgs, 0, "Mira", "User")
	if err != nil {
		t.Fatal(err)
	}
	if cr.DidCompact {
		t.Error("expected no compaction with 10 small messages, but DidCompact=true")
	}
}

func TestMaybeCompact_RealisticSimMessages(t *testing.T) {
	// Simulate the actual message sizes from our compaction stress test.
	// These are the real character counts from the sim run.
	tmpFile, err := os.CreateTemp("", "compact-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := memory.NewStore(tmpFile.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Real message sizes from the compaction stress test (chars):
	charSizes := []int{
		80, 67,   // turn 1
		82, 104,  // turn 2
		106, 61,  // turn 3
		85, 52,   // turn 4
		93, 78,   // turn 5
		281, 217, // turn 6 (chatty phase starts)
		259, 221, // turn 7
		228, 199, // turn 8
		256, 142, // turn 9
		256, 132, // turn 10
		283, 122, // turn 11
		269, 125, // turn 12
		228, 116, // turn 13
		210, 142, // turn 14
		217, 120, // turn 15
	}

	msgs := make([]memory.Message, len(charSizes))
	for i, size := range charSizes {
		content := string(make([]byte, size))
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = memory.Message{
			ID:              int64(i + 1),
			Role:            role,
			ContentRaw:      content,
			ContentScrubbed: content,
		}
	}

	// Check cumulative token count at each message
	threshold := int(float64(1400) * 0.75) // 1050
	for i := 1; i <= len(msgs); i++ {
		tokens := EstimateHistoryTokens("", msgs[:i])
		if tokens >= threshold {
			t.Logf("THRESHOLD CROSSED at message %d (%d tokens >= %d threshold)",
				i, tokens, threshold)
			break
		}
		if i == len(msgs) {
			t.Errorf("never crossed threshold! final tokens: %d (threshold: %d)", tokens, threshold)
		}
	}

	// Full set should be well over threshold
	allTokens := EstimateHistoryTokens("", msgs)
	t.Logf("all %d messages: %d estimated tokens (threshold: %d)", len(msgs), allTokens, threshold)
}

func TestMaybeCompact_OnlyCountsUnsummarized(t *testing.T) {
	// Regression test for runaway re-compaction.
	//
	// Scenario: 40 messages exist in the DB, but messages 1-34 were already
	// summarized in a previous turn. The summary row records end_id=34.
	// Only messages 35-40 (6 messages) are un-summarized.
	//
	// 6 messages × 35 tokens = 210 tokens — well under the 1050 threshold
	// (75% of 1400). Without end_id filtering, the estimator would count all
	// 40 messages (1400 tokens) and fire compaction every single turn forever.
	//
	// This test verifies that MaybeCompact reads summaryEndID from the DB
	// and excludes already-summarized messages from the token estimate.
	tmpFile, err := os.CreateTemp("", "compact-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := memory.NewStore(tmpFile.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Store a summary that covers messages 1–34.
	_, err = store.SaveSummary("test-conv", "Earlier conversation summary.", 1, 34, "chat")
	if err != nil {
		t.Fatal(err)
	}

	// Create 40 messages with IDs 1–40. In a real DB these come from AUTOINCREMENT,
	// but makeMessages gives them IDs 1..N via the loop index.
	msgs := makeMessages(40, 100)
	// msgs[i].ID is set to int64(i+1) by makeMessages — so msgs[34..39] have IDs 35-40.

	cr, err := MaybeCompact(nil, store, "test-conv", msgs, 1400, "Mira", "User")
	if err != nil {
		t.Fatal(err)
	}

	// Only 6 un-summarized messages (IDs 35-40) should be counted: ~210 tokens.
	// That's well under the 1050 threshold, so Triggered must be false.
	if cr.Triggered {
		t.Errorf("expected Triggered=false (only 6 un-summarized msgs, ~210 tokens, threshold=1050), "+
			"but trigger fired — end_id filter not working (runaway compaction regression)")
	}
	if len(cr.KeptMessages) != 40 {
		t.Errorf("expected all 40 messages returned for context, got %d", len(cr.KeptMessages))
	}
}

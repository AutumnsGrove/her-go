package scrub

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Tier 1: Hard Redact
// ---------------------------------------------------------------------------

func TestScrub_SSN_WithDashes(t *testing.T) {
	result := Scrub("my ssn is 123-45-6789 thanks")
	if !strings.Contains(result.Text, "[REDACTED]") {
		t.Errorf("expected SSN to be redacted, got: %s", result.Text)
	}
	if strings.Contains(result.Text, "123-45-6789") {
		t.Error("SSN with dashes was not removed from output")
	}
}

func TestScrub_CreditCard(t *testing.T) {
	// Table-driven: multiple card formats that should all be caught.
	cases := []struct {
		name  string
		input string
	}{
		{"spaces", "card is 4111 1111 1111 1111"},
		{"dashes", "card is 4111-1111-1111-1111"},
		{"no separators", "card is 4111111111111111"},
		{"13 digits (Visa old)", "card is 4222222222222"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("expected card to be redacted, got: %s", result.Text)
			}
		})
	}
}

func TestScrub_APIKeys(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"OpenAI key", "key is sk-abc123def456ghi789jkl012mno345"},
		{"Bearer token", "auth: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abcdef"},
		{"GitHub PAT", "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl"},
		{"AWS key", "access key AKIAIOSFODNN7EXAMPLE"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("expected API key to be redacted, got: %s", result.Text)
			}
		})
	}
}

func TestScrub_Passwords(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"password=", "config: password=hunter2"},
		{"passwd:", "passwd: s3cret!"},
		{"pwd=", "pwd=my_password_123"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("expected password to be redacted, got: %s", result.Text)
			}
		})
	}
}

func TestScrub_BankInfo(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"routing number", "routing number: 021000021"},
		{"ABA number", "ABA# 021000021"},
		{"account number", "account number 12345678901234"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("expected bank info to be redacted, got: %s", result.Text)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tier 2: Tokenize (reversible)
// ---------------------------------------------------------------------------

func TestScrub_PhoneNumber(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		original string // the phone number as it appears in the text
	}{
		{"dashes", "call me at 503-555-1234", "503-555-1234"},
		{"dots", "call me at 503.555.1234", "503.555.1234"},
		{"parens", "call me at (503) 555-1234", "(503) 555-1234"},
		{"+1 prefix", "call me at +1 503-555-1234", "+1 503-555-1234"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[PHONE_1]") {
				t.Errorf("expected [PHONE_1] token, got: %s", result.Text)
			}
			if strings.Contains(result.Text, tc.original) {
				t.Error("original phone number still present in output")
			}
			// Verify vault has the mapping
			entries := result.Vault.Entries()
			if len(entries) != 1 {
				t.Fatalf("expected 1 vault entry, got %d", len(entries))
			}
			if entries[0].Original != tc.original {
				t.Errorf("vault original = %q, want %q", entries[0].Original, tc.original)
			}
			if entries[0].EntityType != "phone" {
				t.Errorf("vault entity type = %q, want %q", entries[0].EntityType, "phone")
			}
		})
	}
}

func TestScrub_Email(t *testing.T) {
	result := Scrub("reach me at autumn@example.com please")

	if !strings.Contains(result.Text, "[EMAIL_1]") {
		t.Errorf("expected [EMAIL_1] token, got: %s", result.Text)
	}
	if strings.Contains(result.Text, "autumn@example.com") {
		t.Error("original email still present in output")
	}

	entries := result.Vault.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 vault entry, got %d", len(entries))
	}
	if entries[0].Original != "autumn@example.com" {
		t.Errorf("vault original = %q, want %q", entries[0].Original, "autumn@example.com")
	}
}

func TestScrub_IPAddress(t *testing.T) {
	result := Scrub("server is at 192.168.1.100 on the LAN")

	if !strings.Contains(result.Text, "[IP_1]") {
		t.Errorf("expected [IP_1] token, got: %s", result.Text)
	}
	if strings.Contains(result.Text, "192.168.1.100") {
		t.Error("original IP still present in output")
	}
}

// ---------------------------------------------------------------------------
// Tier 2: Deduplication — same value twice gets the same token
// ---------------------------------------------------------------------------

func TestScrub_DuplicatePhone(t *testing.T) {
	result := Scrub("call 503-555-1234 or text 503-555-1234")

	// Same number should produce ONE token used twice, not two different tokens.
	if strings.Contains(result.Text, "[PHONE_2]") {
		t.Error("duplicate phone got a second token — should reuse [PHONE_1]")
	}
	count := strings.Count(result.Text, "[PHONE_1]")
	if count != 2 {
		t.Errorf("expected [PHONE_1] to appear twice, appeared %d times", count)
	}
	// Vault should have exactly one entry
	if len(result.Vault.Entries()) != 1 {
		t.Errorf("expected 1 vault entry for duplicate, got %d", len(result.Vault.Entries()))
	}
}

func TestScrub_MultipleDistinctPhones(t *testing.T) {
	result := Scrub("home: 503-555-1111, work: 503-555-2222")

	if !strings.Contains(result.Text, "[PHONE_1]") {
		t.Errorf("expected [PHONE_1], got: %s", result.Text)
	}
	if !strings.Contains(result.Text, "[PHONE_2]") {
		t.Errorf("expected [PHONE_2], got: %s", result.Text)
	}
	if len(result.Vault.Entries()) != 2 {
		t.Errorf("expected 2 vault entries, got %d", len(result.Vault.Entries()))
	}
}

// ---------------------------------------------------------------------------
// Mixed content: Tier 1 + Tier 2 in the same message
// ---------------------------------------------------------------------------

func TestScrub_MixedContent(t *testing.T) {
	input := "my ssn is 123-45-6789 and call me at 503-555-9999 or email me at test@example.com"
	result := Scrub(input)

	// SSN should be hard-redacted (Tier 1)
	if !strings.Contains(result.Text, "[REDACTED]") {
		t.Error("expected SSN to be redacted")
	}
	if strings.Contains(result.Text, "123-45-6789") {
		t.Error("SSN still present")
	}

	// Phone should be tokenized (Tier 2)
	if !strings.Contains(result.Text, "[PHONE_1]") {
		t.Errorf("expected [PHONE_1], got: %s", result.Text)
	}

	// Email should be tokenized (Tier 2)
	if !strings.Contains(result.Text, "[EMAIL_1]") {
		t.Errorf("expected [EMAIL_1], got: %s", result.Text)
	}

	// Vault should have phone + email but NOT the SSN
	entries := result.Vault.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 vault entries (phone + email), got %d", len(entries))
	}
	for _, e := range entries {
		if strings.Contains(e.Original, "123-45-6789") {
			t.Error("SSN should not appear in vault — it's Tier 1 (hard redact)")
		}
	}
}

// ---------------------------------------------------------------------------
// False positives: normal text should pass through unchanged
// ---------------------------------------------------------------------------

func TestScrub_NoFalsePositives(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"plain text", "I went to the store and bought groceries"},
		{"names", "My friend Alice lives in Portland"},
		{"numbers in context", "I ran 5 miles in 42 minutes"},
		{"short numbers", "room 101 on floor 3"},
		{"code snippet", "for i := 0; i < 10; i++ { fmt.Println(i) }"},
		{"URL-like", "check out golang.org/doc for more info"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if result.Text != tc.input {
				t.Errorf("text was modified:\n  input:  %s\n  output: %s", tc.input, result.Text)
			}
			if len(result.Vault.Entries()) != 0 {
				t.Errorf("expected empty vault, got %d entries", len(result.Vault.Entries()))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unicode: non-ASCII content should pass through without corruption
// ---------------------------------------------------------------------------

func TestScrub_Unicode(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"emoji", "I'm feeling great today! \U0001f60a"},
		{"CJK", "today I learned \u4f60\u597d means hello"},
		{"accented", "the caf\u00e9 on Ren\u00e9's street"},
		{"mixed with PII", "caf\u00e9 \u2615 call me at 503-555-7777"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			// For the case with PII, check the phone was tokenized
			if tc.name == "mixed with PII" {
				if !strings.Contains(result.Text, "[PHONE_1]") {
					t.Errorf("expected phone tokenized in unicode text, got: %s", result.Text)
				}
				// The unicode parts should survive
				if !strings.Contains(result.Text, "caf\u00e9") {
					t.Error("unicode text was corrupted")
				}
			} else {
				// No PII — text should be untouched
				if result.Text != tc.input {
					t.Errorf("unicode text was modified:\n  input:  %s\n  output: %s", tc.input, result.Text)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Deanonymize: round-trip — scrub then restore
// ---------------------------------------------------------------------------

func TestDeanonymize_RoundTrip(t *testing.T) {
	// Simulate the full pipeline: user sends PII, we scrub it, LLM responds
	// using the tokens, then we deanonymize back to originals.
	input := "call me at 503-555-1234 or email test@example.com"
	scrubbed := Scrub(input)

	// Simulate LLM response that references the tokens
	llmResponse := "Sure! I'll call you at [PHONE_1] and also send a note to [EMAIL_1]."
	restored := Deanonymize(llmResponse, scrubbed.Vault)

	if !strings.Contains(restored, "503-555-1234") {
		t.Errorf("phone not restored: %s", restored)
	}
	if !strings.Contains(restored, "test@example.com") {
		t.Errorf("email not restored: %s", restored)
	}
	if strings.Contains(restored, "[PHONE_1]") {
		t.Error("token [PHONE_1] still present after deanonymize")
	}
	if strings.Contains(restored, "[EMAIL_1]") {
		t.Error("token [EMAIL_1] still present after deanonymize")
	}
}

func TestDeanonymize_EmptyVault(t *testing.T) {
	// When there are no tokens, text should pass through unchanged.
	vault := NewVault()
	text := "nothing to replace here"
	result := Deanonymize(text, vault)
	if result != text {
		t.Errorf("expected unchanged text, got: %s", result)
	}
}

// ---------------------------------------------------------------------------
// Tier 1 priority: Tier 1 runs first so Tier 2 doesn't partially match
// ---------------------------------------------------------------------------

func TestScrub_Tier1BeforeTier2(t *testing.T) {
	// An SSN contains digit groups that *could* look like a phone prefix.
	// Tier 1 should redact first, leaving nothing for Tier 2 to match.
	result := Scrub("ssn 123-45-6789")

	if strings.Contains(result.Text, "[PHONE") {
		t.Error("SSN was partially matched as a phone number — Tier 1 should run first")
	}
	if !strings.Contains(result.Text, "[REDACTED]") {
		t.Errorf("SSN not redacted, got: %s", result.Text)
	}
	// Vault should be empty — nothing tokenized
	if len(result.Vault.Entries()) != 0 {
		t.Errorf("expected empty vault (SSN is Tier 1, not Tier 2), got %d entries", len(result.Vault.Entries()))
	}
}

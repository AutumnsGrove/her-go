package scrub

import (
	"strings"
	"testing"
)

// =====================================================================
// Tier 1 — Hard redaction (irreversible)
// =====================================================================

func TestScrub_SSN_WithDashes(t *testing.T) {
	result := Scrub("My SSN is 123-45-6789 thanks")
	if strings.Contains(result.Text, "123-45-6789") {
		t.Error("SSN with dashes was not redacted")
	}
	if !strings.Contains(result.Text, "[REDACTED]") {
		t.Error("expected [REDACTED] placeholder")
	}
}

func TestScrub_SSN_NoDashes(t *testing.T) {
	result := Scrub("SSN: 123456789")
	if strings.Contains(result.Text, "123456789") {
		t.Error("SSN without dashes was not redacted")
	}
}

func TestScrub_CreditCard_WithSpaces(t *testing.T) {
	result := Scrub("Card: 4111 1111 1111 1111")
	if strings.Contains(result.Text, "4111") {
		t.Error("credit card with spaces was not redacted")
	}
}

func TestScrub_CreditCard_WithDashes(t *testing.T) {
	result := Scrub("Use card 4111-1111-1111-1111 for payment")
	if strings.Contains(result.Text, "4111") {
		t.Error("credit card with dashes was not redacted")
	}
}

func TestScrub_APIKey_OpenAI(t *testing.T) {
	result := Scrub("my key is sk-abcdefghijklmnopqrstuvwxyz1234")
	if strings.Contains(result.Text, "sk-abcdefghijklmnopqrstuvwxyz1234") {
		t.Error("OpenAI-style API key was not redacted")
	}
}

func TestScrub_APIKey_GitHub(t *testing.T) {
	result := Scrub("token: ghp_abcdefghijklmnopqrstuvwxyz1234567890")
	if strings.Contains(result.Text, "ghp_") {
		t.Error("GitHub PAT was not redacted")
	}
}

func TestScrub_APIKey_AWS(t *testing.T) {
	result := Scrub("key is AKIAIOSFODNN7EXAMPLE")
	if strings.Contains(result.Text, "AKIA") {
		t.Error("AWS access key was not redacted")
	}
}

func TestScrub_BearerToken(t *testing.T) {
	result := Scrub("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.test")
	if strings.Contains(result.Text, "eyJhbGci") {
		t.Error("Bearer token was not redacted")
	}
}

func TestScrub_PasswordAssignment(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"password=", "password=hunter2"},
		{"passwd:", "passwd: s3cret!"},
		{"pwd=", "pwd=mysecretpass"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("password assignment not redacted: %q → %q", tc.input, result.Text)
			}
		})
	}
}

func TestScrub_BankRouting(t *testing.T) {
	result := Scrub("routing number: 021000021")
	if strings.Contains(result.Text, "021000021") {
		t.Error("bank routing number was not redacted")
	}
}

func TestScrub_BankAccount(t *testing.T) {
	result := Scrub("account number: 123456789012")
	if strings.Contains(result.Text, "123456789012") {
		t.Error("bank account number was not redacted")
	}
}

// =====================================================================
// Tier 2 — Tokenization (reversible)
// =====================================================================

func TestScrub_PhoneNumber(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"parens", "Call me at (503) 555-1234"},
		{"dashes", "Call me at 503-555-1234"},
		{"dots", "Call me at 503.555.1234"},
		{"plus1", "Call me at +1 503-555-1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := Scrub(tc.input)
			if !strings.Contains(result.Text, "[PHONE_1]") {
				t.Errorf("phone not tokenized: %q → %q", tc.input, result.Text)
			}
			if len(result.Vault.Entries()) == 0 {
				t.Fatal("vault is empty — phone not stored")
			}
			entry := result.Vault.Entries()[0]
			if entry.EntityType != "phone" {
				t.Errorf("entityType = %q, want %q", entry.EntityType, "phone")
			}
		})
	}
}

func TestScrub_Email(t *testing.T) {
	result := Scrub("reach me at autumn@example.com please")
	if !strings.Contains(result.Text, "[EMAIL_1]") {
		t.Errorf("email not tokenized: %q", result.Text)
	}
	if strings.Contains(result.Text, "autumn@example.com") {
		t.Error("raw email still present after scrubbing")
	}
	entries := result.Vault.Entries()
	if len(entries) != 1 {
		t.Fatalf("vault has %d entries, want 1", len(entries))
	}
	if entries[0].Original != "autumn@example.com" {
		t.Errorf("vault original = %q, want %q", entries[0].Original, "autumn@example.com")
	}
}

func TestScrub_IP(t *testing.T) {
	result := Scrub("server is at 192.168.1.100")
	if !strings.Contains(result.Text, "[IP_1]") {
		t.Errorf("IP not tokenized: %q", result.Text)
	}
}

func TestScrub_DuplicatePhone_SameToken(t *testing.T) {
	result := Scrub("call 503-555-1234 or text 503-555-1234")

	count := strings.Count(result.Text, "[PHONE_1]")
	if count != 2 {
		t.Errorf("expected [PHONE_1] to appear twice, got %d in: %q", count, result.Text)
	}
	// Should only have 1 vault entry (not 2)
	if len(result.Vault.Entries()) != 1 {
		t.Errorf("vault has %d entries, want 1 (dedup same number)", len(result.Vault.Entries()))
	}
}

func TestScrub_MultiplePhones_DifferentTokens(t *testing.T) {
	result := Scrub("home: 503-555-1111, work: 503-555-2222")
	if !strings.Contains(result.Text, "[PHONE_1]") {
		t.Errorf("first phone not tokenized: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[PHONE_2]") {
		t.Errorf("second phone not tokenized: %q", result.Text)
	}
	if len(result.Vault.Entries()) != 2 {
		t.Errorf("vault has %d entries, want 2", len(result.Vault.Entries()))
	}
}

// =====================================================================
// Tier 3 — Pass through (no false positives)
// =====================================================================

func TestScrub_NormalText_Unchanged(t *testing.T) {
	inputs := []string{
		"Hey, how's your day going?",
		"I went to the store and bought some coffee",
		"My name is Autumn and I live in Portland",
		"The meeting is at 3pm tomorrow",
		"I scored 95 on my test",
	}
	for _, text := range inputs {
		result := Scrub(text)
		if result.Text != text {
			t.Errorf("normal text was modified:\n  input:  %q\n  output: %q", text, result.Text)
		}
		if len(result.Vault.Entries()) != 0 {
			t.Errorf("vault should be empty for normal text, got %d entries", len(result.Vault.Entries()))
		}
	}
}

// =====================================================================
// Mixed content
// =====================================================================

func TestScrub_MixedContent(t *testing.T) {
	input := "My SSN is 123-45-6789, call me at 503-555-1234, email autumn@test.com"
	result := Scrub(input)

	// SSN should be hard redacted
	if strings.Contains(result.Text, "123-45-6789") {
		t.Error("SSN was not redacted")
	}
	// Phone should be tokenized
	if !strings.Contains(result.Text, "[PHONE_1]") {
		t.Errorf("phone not tokenized in mixed content: %q", result.Text)
	}
	// Email should be tokenized
	if !strings.Contains(result.Text, "[EMAIL_1]") {
		t.Errorf("email not tokenized in mixed content: %q", result.Text)
	}
}

func TestScrub_Tier1BeforeTier2(t *testing.T) {
	// Tier 1 should run first — a credit card number shouldn't be
	// partially matched as a phone number by Tier 2.
	result := Scrub("card 4111111111111111")
	if strings.Contains(result.Text, "[PHONE") {
		t.Errorf("credit card was tokenized as phone instead of redacted: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[REDACTED]") {
		t.Errorf("credit card was not hard redacted: %q", result.Text)
	}
}

// =====================================================================
// Deanonymize
// =====================================================================

func TestDeanonymize_ReplacesTokens(t *testing.T) {
	// Simulate what happens: scrub a message, send scrubbed to LLM,
	// LLM responds with tokens, then deanonymize the response.
	scrubbed := Scrub("call me at 503-555-9876")

	// LLM response uses the token
	llmResponse := "Sure! I'll remember your number [PHONE_1]."
	restored := Deanonymize(llmResponse, scrubbed.Vault)

	if !strings.Contains(restored, "503-555-9876") {
		t.Errorf("deanonymize failed: %q", restored)
	}
	if strings.Contains(restored, "[PHONE_1]") {
		t.Error("token was not replaced during deanonymization")
	}
}

func TestDeanonymize_MultipleTokens(t *testing.T) {
	scrubbed := Scrub("email: foo@bar.com, phone: 503-555-0000")

	response := "Got it: [EMAIL_1] and [PHONE_1]"
	restored := Deanonymize(response, scrubbed.Vault)

	if strings.Contains(restored, "[EMAIL_1]") || strings.Contains(restored, "[PHONE_1]") {
		t.Errorf("not all tokens replaced: %q", restored)
	}
}

func TestDeanonymize_NoTokens_PassThrough(t *testing.T) {
	vault := NewVault()
	result := Deanonymize("nothing to replace here", vault)
	if result != "nothing to replace here" {
		t.Errorf("text was modified when no tokens exist: %q", result)
	}
}

// =====================================================================
// Vault unit tests
// =====================================================================

func TestVault_FindByOriginal(t *testing.T) {
	v := NewVault()
	v.Add("[PHONE_1]", "503-555-1234", "phone")
	v.Add("[EMAIL_1]", "a@b.com", "email")

	token, ok := v.FindByOriginal("503-555-1234", "phone")
	if !ok {
		t.Fatal("FindByOriginal did not find phone entry")
	}
	if token != "[PHONE_1]" {
		t.Errorf("token = %q, want %q", token, "[PHONE_1]")
	}

	// Wrong entity type should not match
	_, ok = v.FindByOriginal("503-555-1234", "email")
	if ok {
		t.Error("FindByOriginal matched wrong entity type")
	}
}

func TestVault_CountByType(t *testing.T) {
	v := NewVault()
	v.Add("[PHONE_1]", "111", "phone")
	v.Add("[PHONE_2]", "222", "phone")
	v.Add("[EMAIL_1]", "a@b.com", "email")

	if v.CountByType("phone") != 2 {
		t.Errorf("phone count = %d, want 2", v.CountByType("phone"))
	}
	if v.CountByType("email") != 1 {
		t.Errorf("email count = %d, want 1", v.CountByType("email"))
	}
	if v.CountByType("ip") != 0 {
		t.Errorf("ip count = %d, want 0", v.CountByType("ip"))
	}
}

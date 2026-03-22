// Package scrub implements tiered PII detection and scrubbing.
//
// Tier 1: Hard redact — SSNs, credit cards, API keys → replaced with [REDACTED]
// Tier 2: Tokenize — phone numbers, emails, addresses, IPs → replaced with
//
//	numbered placeholders like [PHONE_1], reversible via the Vault
//
// Tier 3: Pass through — names, places, context → left intact for coherence
package scrub

import (
	"fmt"
	"regexp"
	"strings"
)

// ScrubResult holds the scrubbed text and a vault of Tier 2 token mappings
// that can be used to deanonymize the LLM's response.
type ScrubResult struct {
	Text  string // the scrubbed version of the input
	Vault *Vault // token↔original mappings for deanonymization
}

// tier1Pattern groups all Tier 1 (hard redact) regex patterns.
// These match things that should NEVER leave the machine.
// Each pattern is compiled once at package init — MustCompile panics on
// bad regex, which is fine since these are constants.
var tier1Patterns = []*regexp.Regexp{
	// SSN: XXX-XX-XXXX (with or without dashes)
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
	regexp.MustCompile(`\b\d{9}\b`), // SSN without dashes (9 consecutive digits)

	// Credit/debit card numbers (13-19 digits, optionally separated by spaces/dashes)
	regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`),

	// API keys and secrets (common patterns)
	regexp.MustCompile(`\b(?:sk-[a-zA-Z0-9]{20,})\b`),                   // OpenAI/Anthropic style
	regexp.MustCompile(`\b(?:Bearer\s+[a-zA-Z0-9._\-]{20,})\b`),         // Bearer tokens
	regexp.MustCompile(`\b(?:ghp_[a-zA-Z0-9]{36,})\b`),                  // GitHub PAT
	regexp.MustCompile(`\b(?:AKIA[A-Z0-9]{16})\b`),                      // AWS access key
	regexp.MustCompile(`\b(?:password|passwd|pwd)\s*[:=]\s*\S+`),         // password assignments

	// Bank routing numbers (9 digits, but we need the routing context)
	regexp.MustCompile(`(?i)\b(?:routing|aba)\s*(?:#|number|num|no)?[:\s]*\d{9}\b`),
	regexp.MustCompile(`(?i)\b(?:account)\s*(?:#|number|num|no)?[:\s]*\d{8,17}\b`),
}

// tier2Defs defines Tier 2 patterns and their token prefixes.
// These get replaced with numbered placeholders and stored in the vault.
type tier2Def struct {
	pattern    *regexp.Regexp
	entityType string // "phone", "email", "address", "ip"
	prefix     string // "PHONE", "EMAIL", "ADDRESS", "IP"
}

var tier2Defs = []tier2Def{
	{
		// Phone numbers: various US formats
		// (xxx) xxx-xxxx, xxx-xxx-xxxx, xxx.xxx.xxxx, +1xxxxxxxxxx, etc.
		pattern:    regexp.MustCompile(`(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`),
		entityType: "phone",
		prefix:     "PHONE",
	},
	{
		// Email addresses
		pattern:    regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`),
		entityType: "email",
		prefix:     "EMAIL",
	},
	{
		// IP addresses (IPv4)
		pattern:    regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
		entityType: "ip",
		prefix:     "IP",
	},
}

// Scrub applies tiered PII scrubbing to the input text.
// It first applies Tier 1 (hard redact), then Tier 2 (tokenize).
// Tier 3 items (names, places) pass through untouched.
//
// The returned ScrubResult contains the scrubbed text and a Vault
// that maps Tier 2 tokens back to their originals for deanonymization.
func Scrub(text string) *ScrubResult {
	vault := NewVault()

	// Tier 1: Hard redact — replace with [REDACTED], no recovery possible.
	// We do this first so Tier 2 patterns don't accidentally match
	// parts of SSNs or card numbers.
	scrubbed := text
	for _, pattern := range tier1Patterns {
		scrubbed = pattern.ReplaceAllString(scrubbed, "[REDACTED]")
	}

	// Tier 2: Tokenize — replace with numbered placeholders, store mapping.
	// Each entity type gets its own counter: [PHONE_1], [PHONE_2], etc.
	for _, def := range tier2Defs {
		scrubbed = def.pattern.ReplaceAllStringFunc(scrubbed, func(match string) string {
			// Check if already in vault (same value seen before in this message)
			if token, exists := vault.FindByOriginal(match, def.entityType); exists {
				return token
			}

			// New value — assign next number and store in vault
			token := fmt.Sprintf("[%s_%d]", def.prefix, vault.CountByType(def.entityType)+1)
			vault.Add(token, match, def.entityType)
			return token
		})
	}

	return &ScrubResult{
		Text:  scrubbed,
		Vault: vault,
	}
}

// Deanonymize replaces Tier 2 tokens in an LLM response with their
// original values from the vault. This is how "[PHONE_1]" in the bot's
// reply becomes the actual phone number the user sees.
func Deanonymize(text string, vault *Vault) string {
	result := text
	// Walk through all vault entries and replace tokens with originals.
	// We iterate in reverse order of token length to avoid partial replacements
	// (e.g., [PHONE_10] shouldn't partially match [PHONE_1]).
	for _, entry := range vault.Entries() {
		result = strings.ReplaceAll(result, entry.Token, entry.Original)
	}
	return result
}

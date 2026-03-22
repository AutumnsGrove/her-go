package scrub

// VaultEntry maps a single placeholder token to its original value.
// For example: Token="[PHONE_1]", Original="503-555-1234", EntityType="phone"
type VaultEntry struct {
	Token      string
	Original   string
	EntityType string
}

// Vault is a session-scoped mapping of Tier 2 tokens to original values.
// It's created fresh for each message, used to deanonymize the LLM response,
// and then persisted to SQLite for audit trail.
//
// This is a simple struct with a slice (Go's dynamic array — like a Python list
// but typed). We use a slice instead of a map because we need to preserve
// insertion order and iterate predictably.
type Vault struct {
	entries []VaultEntry
}

// NewVault creates an empty vault for a new scrubbing session.
func NewVault() *Vault {
	return &Vault{
		entries: make([]VaultEntry, 0),
	}
}

// Add stores a new token↔original mapping in the vault.
func (v *Vault) Add(token, original, entityType string) {
	v.entries = append(v.entries, VaultEntry{
		Token:      token,
		Original:   original,
		EntityType: entityType,
	})
}

// FindByOriginal checks if a given original value already has a token
// assigned for the specified entity type. This prevents duplicate tokens
// when the same phone number or email appears multiple times in one message.
func (v *Vault) FindByOriginal(original, entityType string) (string, bool) {
	for _, entry := range v.entries {
		if entry.Original == original && entry.EntityType == entityType {
			return entry.Token, true
		}
	}
	// In Go, functions that do lookups often return (value, bool) — the bool
	// indicates whether the value was found. This is called the "comma ok"
	// idiom and you'll see it with maps too: val, ok := myMap[key]
	return "", false
}

// CountByType returns how many entries exist for a given entity type.
// Used to generate the next numbered token (e.g., if count is 2, next is [PHONE_3]).
func (v *Vault) CountByType(entityType string) int {
	count := 0
	for _, entry := range v.entries {
		if entry.EntityType == entityType {
			count++
		}
	}
	return count
}

// Entries returns all vault entries. Used for deanonymization and
// persistence to the pii_vault SQLite table.
func (v *Vault) Entries() []VaultEntry {
	return v.entries
}

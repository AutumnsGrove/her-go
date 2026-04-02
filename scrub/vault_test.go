package scrub

import (
	"testing"
)

func TestVault_NewVault(t *testing.T) {
	v := NewVault()
	if v == nil {
		t.Fatal("NewVault returned nil")
	}
	if len(v.Entries()) != 0 {
		t.Errorf("new vault should be empty, got %d entries", len(v.Entries()))
	}
}

func TestVault_Add(t *testing.T) {
	v := NewVault()
	v.Add("[PHONE_1]", "503-555-1234", "phone")

	entries := v.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Token != "[PHONE_1]" {
		t.Errorf("token = %q, want %q", entries[0].Token, "[PHONE_1]")
	}
	if entries[0].Original != "503-555-1234" {
		t.Errorf("original = %q, want %q", entries[0].Original, "503-555-1234")
	}
	if entries[0].EntityType != "phone" {
		t.Errorf("entityType = %q, want %q", entries[0].EntityType, "phone")
	}
}

func TestVault_FindByOriginal(t *testing.T) {
	v := NewVault()
	v.Add("[PHONE_1]", "503-555-1234", "phone")
	v.Add("[EMAIL_1]", "test@example.com", "email")

	t.Run("found", func(t *testing.T) {
		token, ok := v.FindByOriginal("503-555-1234", "phone")
		if !ok {
			t.Fatal("expected to find phone entry")
		}
		if token != "[PHONE_1]" {
			t.Errorf("token = %q, want %q", token, "[PHONE_1]")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := v.FindByOriginal("999-999-9999", "phone")
		if ok {
			t.Error("expected not found for unknown number")
		}
	})

	t.Run("wrong entity type", func(t *testing.T) {
		// The original exists but under a different entity type —
		// should not match. This prevents cross-type collisions.
		_, ok := v.FindByOriginal("503-555-1234", "email")
		if ok {
			t.Error("should not find phone number under email entity type")
		}
	})
}

func TestVault_CountByType(t *testing.T) {
	v := NewVault()
	v.Add("[PHONE_1]", "503-555-1111", "phone")
	v.Add("[PHONE_2]", "503-555-2222", "phone")
	v.Add("[EMAIL_1]", "a@b.com", "email")

	if got := v.CountByType("phone"); got != 2 {
		t.Errorf("phone count = %d, want 2", got)
	}
	if got := v.CountByType("email"); got != 1 {
		t.Errorf("email count = %d, want 1", got)
	}
	if got := v.CountByType("ip"); got != 0 {
		t.Errorf("ip count = %d, want 0", got)
	}
}

func TestVault_Entries_PreservesOrder(t *testing.T) {
	// Insertion order matters for deanonymization — longer tokens
	// should be replaced before shorter ones to avoid partial matches.
	v := NewVault()
	v.Add("[PHONE_1]", "first", "phone")
	v.Add("[EMAIL_1]", "second", "email")
	v.Add("[PHONE_2]", "third", "phone")

	entries := v.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify insertion order is preserved
	expected := []string{"first", "second", "third"}
	for i, want := range expected {
		if entries[i].Original != want {
			t.Errorf("entry[%d].Original = %q, want %q", i, entries[i].Original, want)
		}
	}
}

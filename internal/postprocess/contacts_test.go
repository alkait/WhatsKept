package postprocess

import (
	"reflect"
	"sort"
	"testing"
)

// These tests cover the pure-logic functions of contacts.go — no
// SQLite, no backup decryption, no temp files. They mirror the
// behavioural contract of `whatskept.contacts` (Python source-of-
// truth) so a regression in the Go port shows up immediately.

// ---------------------------------------------------------------------------
// composeDisplayName
// ---------------------------------------------------------------------------

func TestComposeDisplayName(t *testing.T) {
	cases := []struct {
		name                           string
		first, middle, last, org, nick string
		want                           string
	}{
		{"nickname wins", "John", "", "Doe", "Acme", "Mom", "Mom"},
		{"trims nickname whitespace", "", "", "", "", "  Mom  ", "Mom"},
		{"first+last", "Jane", "", "Doe", "", "", "Jane Doe"},
		{"first+middle+last", "Jane", "Q", "Doe", "", "", "Jane Q Doe"},
		{"first only", "Jane", "", "", "", "", "Jane"},
		{"last only", "", "", "Doe", "", "", "Doe"},
		{"org fallback when no person name", "", "", "", "Acme Corp", "", "Acme Corp"},
		{"trims org whitespace", "", "", "", "  Acme  ", "", "Acme"},
		{"empty everything", "", "", "", "", "", ""},
		{"whitespace-only fields", "  ", "  ", "  ", "  ", "  ", ""},
		{"unicode names", "山田", "", "太郎", "", "", "山田 太郎"},
		{"arabic", "محمد", "", "", "", "", "محمد"},
		{"person beats org when both set", "Jane", "", "Doe", "Acme", "", "Jane Doe"},
		{"empty nickname falls through", "Jane", "", "Doe", "", "", "Jane Doe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := composeDisplayName(c.first, c.middle, c.last, c.org, c.nick)
			if got != c.want {
				t.Errorf("composeDisplayName(%q,%q,%q,%q,%q) = %q, want %q",
					c.first, c.middle, c.last, c.org, c.nick, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// normalizePhone
// ---------------------------------------------------------------------------

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"+971 50 123 4567", "971501234567"},
		{"+1-555-867-5309", "15558675309"},
		{"00971501234567", "971501234567"},     // 00 prefix stripped
		{"00 971 50 123 4567", "971501234567"}, // 00 with spaces
		{"(212) 555-1234", "2125551234"},
		{"050-1234567", "0501234567"}, // leading 0 preserved (not E.164)
		// We don't model DTMF tails specially — the extension's digits
		// are kept (Python does the same: just strips non-digits).
		{"+1 (555) 867-5309 ext. 42", "1555867530942"},
		{"123,456,7890", "1234567890"},
		{"5551234", "5551234"}, // 7 digits — minimum
		{"", ""},
		{"abc", ""},
		{"123", ""},              // < min digits
		{"1234567890123456", ""}, // > max digits (16)
		{"+", ""},                // bare plus
		{"  +971 50 1234567  ", "971501234567"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got := normalizePhone(c.raw)
			if got != c.want {
				t.Errorf("normalizePhone(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isCCPrefixed
// ---------------------------------------------------------------------------

func TestIsCCPrefixed(t *testing.T) {
	cases := []struct {
		digits string
		want   bool
	}{
		{"971501234567", true}, // UAE (3-digit CC)
		{"15558675309", true},  // US (1-digit CC '1')
		{"447700123456", true}, // UK (2-digit CC)
		{"491701234567", true}, // DE
		{"0501234567", false},  // local format (leading 0)
		// '55' is Brazil's 2-digit CC — isCCPrefixed returns true.
		// Real-world implication: a Brazilian number saved as bare
		// digits is treated as E.164, not local-format.
		{"5551234567", true},
		{"", false},
		{"1", true},      // bare CC, length-wise nonsensical but still prefixed
		{"99999", false}, // not a real CC
	}
	for _, c := range cases {
		t.Run(c.digits, func(t *testing.T) {
			got := isCCPrefixed(c.digits)
			if got != c.want {
				t.Errorf("isCCPrefixed(%q) = %v, want %v", c.digits, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// applyDefaultCountryCode
// ---------------------------------------------------------------------------

func TestApplyDefaultCountryCode(t *testing.T) {
	cases := []struct {
		name      string
		digits    string
		defaultCC string
		want      string
	}{
		{"already E.164 is returned as-is", "971501234567", "971", "971501234567"},
		// Local-format input: leading 0 + UAE-style 9 digits. The
		// 0-prefix is the trunk-prefix indicator we strip. We can't
		// use a bare "501234567" because '501' is Belize — the
		// matcher would consider it already E.164.
		{"local prefixes default CC", "0501234567", "971", "971501234567"},
		{"strips multiple leading zeros", "00501234567", "971", "971501234567"},
		{"no defaultCC and not E.164 → empty", "0501234567", "", ""},
		// '1' IS a valid CC ('1' = US/Canada), so it's returned
		// as-is via the E.164-prefixed branch (no length check on
		// that path — mirrors Python). normalizePhone would have
		// dropped it earlier in the real pipeline anyway.
		{"single-digit CC returned as-is", "1", "971", "1"},
		{"too long after fixup", "999999999999999", "971", ""},
		{"empty input", "", "971", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyDefaultCountryCode(c.digits, c.defaultCC)
			if got != c.want {
				t.Errorf("applyDefaultCountryCode(%q, %q) = %q, want %q",
					c.digits, c.defaultCC, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// matchContacts
// ---------------------------------------------------------------------------

func TestMatchContacts(t *testing.T) {
	// Universe of "WhatsApp users" the matcher should recognise. JID
	// digits only — caller normalises.
	jids := setOf(
		"971501234567", // UAE saved contact
		"15558675309",  // US saved contact
		"447700900900", // UK saved contact
	)

	contacts := []contact{
		{personID: 1, displayName: "Mom", phones: []string{"+971 50 123 4567"}},
		{personID: 2, displayName: "Dad", phones: []string{"00971501234567"}}, // duplicate JID — first-seen (Mom) wins
		{personID: 3, displayName: "Sis", phones: []string{"050-1234567"}},    // local format, default-CC fixup
		{personID: 4, displayName: "USA", phones: []string{"+1 (555) 867-5309"}},
		{personID: 5, displayName: "UK", phones: []string{"+44 7700 900900"}},
		{personID: 6, displayName: "Junk", phones: []string{"123"}},                                 // too short
		{personID: 7, displayName: "Multi", phones: []string{"+971 9 999 9999", "+1 555 867 5309"}}, // 2nd phone collides w/ USA
		{personID: 8, displayName: "Empty", phones: []string{""}},
	}

	mapping, personIDByJID, stats := matchContacts(contacts, jids, "971")

	// Mom should win JID 971501234567 (first contact iterated;
	// matchContacts iterates the input slice in order so this is
	// deterministic for our test fixture).
	if got := mapping["971501234567"]; got != "Mom" {
		t.Errorf("971501234567 → %q, want Mom (first-seen wins)", got)
	}
	if got := mapping["15558675309"]; got != "USA" {
		t.Errorf("15558675309 → %q, want USA", got)
	}
	if got := mapping["447700900900"]; got != "UK" {
		t.Errorf("447700900900 → %q, want UK", got)
	}

	// Local-format Sis ("050-1234567") should resolve to UAE JID
	// via default-CC fixup, but Mom got there first → it should NOT
	// overwrite Mom.
	if got := mapping["971501234567"]; got != "Mom" {
		t.Errorf("after Sis, 971501234567 = %q, want Mom (no overwrite)", got)
	}

	// Person IDs should track the WINNING contact, not the latest.
	if got := personIDByJID["971501234567"]; got != 1 {
		t.Errorf("personIDByJID[971501234567] = %d, want 1 (Mom)", got)
	}
	if got := personIDByJID["15558675309"]; got != 4 {
		t.Errorf("personIDByJID[15558675309] = %d, want 4 (USA)", got)
	}

	// Junk + Empty are dropped at the normalization stage.
	if _, ok := mapping["123"]; ok {
		t.Errorf("Junk should not appear in mapping")
	}

	// Statistics sanity:
	//   phonesTotal  = 9  (sum of Phones across all 8 contacts)
	if stats.phonesTotal != 9 {
		t.Errorf("phonesTotal = %d, want 9", stats.phonesTotal)
	}
	//   normalised   = 7 (drops "" and "123")
	if stats.phonesNormalized != 7 {
		t.Errorf("phonesNormalized = %d, want 7", stats.phonesNormalized)
	}
	//   matched      = 5 — Mom + Dad (both hit UAE), Sis (UAE), USA, UK, Multi-phone#2 (USA again)
	//   = 6 phone-level hits, but JID-level collisions don't reduce phonesMatched in our impl
	//   = Mom(1) + Dad(1, duplicate) + Sis(1, duplicate) + USA(1) + UK(1) + Multi#2(1, duplicate USA)
	if stats.phonesMatched < 5 {
		t.Errorf("phonesMatched = %d, want >= 5", stats.phonesMatched)
	}

	// Resolved JIDs = 3 distinct.
	resolvedJIDs := keys(mapping)
	sort.Strings(resolvedJIDs)
	want := []string{"15558675309", "447700900900", "971501234567"}
	if !reflect.DeepEqual(resolvedJIDs, want) {
		t.Errorf("resolved JIDs = %v, want %v", resolvedJIDs, want)
	}
}

// ---------------------------------------------------------------------------
// matchContacts: no default CC at all
// ---------------------------------------------------------------------------

// When the JID universe is too small for confident CC detection
// (detectDefaultCountryCode returns ""), we should still match
// every E.164-prefixed phone but skip local-format ones rather than
// guessing wrong.
func TestMatchContacts_NoDefaultCC(t *testing.T) {
	jids := setOf("971501234567", "15558675309")
	contacts := []contact{
		{personID: 1, displayName: "Mom", phones: []string{"+971 50 123 4567"}},
		{personID: 2, displayName: "Local", phones: []string{"050-1234567"}},
	}
	mapping, _, _ := matchContacts(contacts, jids, "" /* no default CC */)

	if got := mapping["971501234567"]; got != "Mom" {
		t.Errorf("Mom should still match by E.164 form, got %q", got)
	}
	if _, ok := mapping["971501234567"]; !ok {
		t.Errorf("Mom should be in mapping")
	}
	// "Local" can't be resolved — local format with no default CC.
	// It should NOT clobber Mom's slot.
	if mapping["971501234567"] == "Local" {
		t.Errorf("Local should not match without default CC")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func setOf(items ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, s := range items {
		out[s] = struct{}{}
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

package postprocess

import "testing"

func TestUpsertCodexTopLevel(t *testing.T) {
	const wantApproval = `approval_policy = "never"`
	const wantSandbox = `sandbox_mode = "danger-full-access"   # or "workspace-write" for safer option`

	t.Run("empty file gets both keys", func(t *testing.T) {
		out := upsertCodexTopLevel("")
		if !contains(out, wantApproval) || !contains(out, wantSandbox) {
			t.Fatalf("missing keys in:\n%s", out)
		}
	})

	t.Run("replaces existing assignments in place", func(t *testing.T) {
		in := "approval_policy = \"on-request\"\nsandbox_mode = \"read-only\"\nmodel = \"gpt-5\"\n"
		out := upsertCodexTopLevel(in)
		if !contains(out, wantApproval) || !contains(out, wantSandbox) {
			t.Fatalf("keys not updated:\n%s", out)
		}
		if contains(out, "on-request") || contains(out, "read-only") {
			t.Fatalf("old values not replaced:\n%s", out)
		}
		if !contains(out, `model = "gpt-5"`) {
			t.Fatalf("user key not preserved:\n%s", out)
		}
		// No duplicate active assignments.
		if n := count(out, "approval_policy ="); n != 1 {
			t.Fatalf("approval_policy assigned %d times:\n%s", n, out)
		}
	})

	t.Run("inserts before existing table and preserves it", func(t *testing.T) {
		in := "[mcp_servers.foo]\ncommand = \"x\"\n"
		out := upsertCodexTopLevel(in)
		// Our keys must come before the table header (top-level TOML rule).
		iApproval := index(out, "approval_policy")
		iTable := index(out, "[mcp_servers.foo]")
		if iApproval < 0 || iTable < 0 || iApproval > iTable {
			t.Fatalf("top-level keys not before table:\n%s", out)
		}
		if !contains(out, `command = "x"`) {
			t.Fatalf("table body not preserved:\n%s", out)
		}
	})
}

// tiny string helpers to keep the test dependency-free
func contains(s, sub string) bool { return index(s, sub) >= 0 }

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func count(s, sub string) int {
	n, i := 0, 0
	for {
		j := index(s[i:], sub)
		if j < 0 {
			return n
		}
		n++
		i += j + len(sub)
	}
}

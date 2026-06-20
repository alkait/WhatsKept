package app

import "testing"

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Plain stable comparisons.
		{"v0.1.0", "v0.2.0", true},
		{"0.1.0", "0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.2.0", "v0.2.0", false},
		{"v0.9.0", "v0.10.0", true},  // numeric, not lexical
		{"v1.0.0", "v0.9.9", false},  // major dominates
		{"v0.1.1", "v0.1.10", true},  // patch numeric
		// Mixed leading-v normalization.
		{"0.1.0", "v0.2.0", true},
		{"v0.1.0", "0.2.0", true},
		// Dev builds carry -prerelease/+build metadata; only the core
		// counts, so a 0.0.0-dev build is below any real release.
		{"0.0.0-dev.12+abc1234", "v0.1.0", true},
		{"v0.1.0", "0.0.0-dev.12+abc1234", false},
		{"v0.1.0-rc.1", "v0.1.0", false}, // same core → not less
		// Unparseable input never reports "less" (stay quiet).
		{"garbage", "v0.2.0", false},
		{"v0.1.0", "garbage", false},
		{"", "v0.2.0", false},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestIsLocalDevBuild(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// Untagged local/dev builds — stay quiet (no update pill).
		{"", true},
		{"0.0.0-dev", true},              // plain `make build` default
		{"v0.0.0-dev", true},             // tolerated leading "v"
		{"0.0.0-dev-c31fa82", true},      // Windows test build with short sha
		{"0.0.0-dev.67+c31fa82", true},   // CI dev form
		// Real tagged releases — prompt for updates.
		{"0.1.0", false},
		{"v2.1.2", false},
		{"1.0.0-rc.1", false},
	}
	for _, c := range cases {
		if got := isLocalDevBuild(c.v); got != c.want {
			t.Errorf("isLocalDevBuild(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

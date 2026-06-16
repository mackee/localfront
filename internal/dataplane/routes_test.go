package dataplane

import (
	"testing"

	"github.com/mackee/localfront/internal/config"
)

// Aliases are case-insensitive hostnames, and match() looks them up with a
// lowercased host, so an alias declared with uppercase letters must still match.
func TestBuildRoutes_AliasCaseInsensitive(t *testing.T) {
	dist := &config.Distribution{
		LogicalID:  "D1",
		ID:         "D1",
		DomainName: "d1.cloudfront.localhost",
		Aliases:    []string{"CDN.Example.COM", "*.Assets.Example.COM"},
		Enabled:    true,
	}
	rt := buildRoutes(&config.Config{Distributions: []*config.Distribution{dist}})

	tests := []struct {
		host string
		want bool
	}{
		{"cdn.example.com", true},        // exact alias, differing case
		{"img.assets.example.com", true}, // wildcard alias, differing case
		{"other.example.com", false},
	}
	for _, tc := range tests {
		got := rt.match(tc.host) != nil
		if got != tc.want {
			t.Errorf("match(%q) matched=%v, want %v", tc.host, got, tc.want)
		}
	}
}

// Disabled distributions are never routed.
func TestBuildRoutes_SkipsDisabled(t *testing.T) {
	dist := &config.Distribution{
		LogicalID:  "D1",
		ID:         "D1",
		DomainName: "d1.cloudfront.localhost",
		Aliases:    []string{"off.example.com"},
		Enabled:    false,
	}
	rt := buildRoutes(&config.Config{Distributions: []*config.Distribution{dist}})
	if rt.match("off.example.com") != nil {
		t.Error("disabled distribution should not be routed")
	}
}

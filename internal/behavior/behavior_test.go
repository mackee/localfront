package behavior

import (
	"testing"

	"github.com/mackee/localfront/internal/config"
)

func TestMatchPath(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// AWS documentation examples.
		{"*", "/", true},
		{"*", "/anything/at/all.html", true},
		{"*.jpg", "/photo.jpg", true},
		{"*.jpg", "/images/photo.jpg", true}, // * spans '/'
		{"*.jpg", "/photo.jpeg", false},
		{"*.jpg", "/photojpg", false},
		{"images/*.jpg", "/images/photo.jpg", true},
		{"images/*.jpg", "/images/sub/photo.jpg", true},
		{"images/*.jpg", "/other/photo.jpg", false},
		{"/images/*", "/images/photo.jpg", true},
		{"/images/*", "/images/", true},
		{"/images/*", "/images", false}, // pattern requires the trailing slash
		{"*.gif", "/a.gif", true},
		{"*.gif", "/a/b/c.gif", true},

		// '?' matches exactly one character.
		{"file?.txt", "/file1.txt", true},
		{"file?.txt", "/file12.txt", false},
		{"file?.txt", "/file.txt", false},

		// Leading slash on the pattern is optional and equivalent.
		{"api/*", "/api/users", true},
		{"/api/*", "/api/users", true},
		{"api/*", "/apix/users", false},

		// Case sensitivity.
		{"/Images/*", "/images/photo.jpg", false},
		{"/images/*", "/Images/photo.jpg", false},

		// Exact, wildcard-free patterns.
		{"/robots.txt", "/robots.txt", true},
		{"/robots.txt", "/robots.txt/extra", false},
	}
	for _, tc := range tests {
		if got := MatchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestSelect_FirstMatchWins(t *testing.T) {
	def := &config.Behavior{PathPattern: ""}
	apiImages := &config.Behavior{PathPattern: "/api/images/*"}
	api := &config.Behavior{PathPattern: "/api/*"}
	gifs := &config.Behavior{PathPattern: "*.gif"}

	// Listing order, not specificity: /api/* is listed before /api/images/*.
	dist := &config.Distribution{
		DefaultBehavior: def,
		Behaviors:       []*config.Behavior{api, apiImages, gifs},
	}

	tests := []struct {
		path string
		want *config.Behavior
	}{
		{"/api/images/cat.gif", api}, // first match wins over the more specific apiImages
		{"/api/users", api},          //
		{"/banner.gif", gifs},        //
		{"/index.html", def},         // falls back to the default
		{"/", def},                   //
	}
	for _, tc := range tests {
		if got := Select(dist, tc.path); got != tc.want {
			t.Errorf("Select(%q) = %q, want %q", tc.path, got.PathPattern, tc.want.PathPattern)
		}
	}
}

func TestSelect_NoBehaviorsReturnsDefault(t *testing.T) {
	def := &config.Behavior{}
	dist := &config.Distribution{DefaultBehavior: def}
	if got := Select(dist, "/anything"); got != def {
		t.Errorf("Select with no cache behaviors should return the default behavior")
	}
}

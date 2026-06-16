package behavior

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
		name := fmt.Sprintf("%s_vs_%s", tc.pattern, tc.path)
		t.Run(name, func(t *testing.T) {
			if got := MatchPath(tc.pattern, tc.path); got != tc.want {
				t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
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
		t.Run(tc.path, func(t *testing.T) {
			if got := Select(dist, tc.path); got != tc.want {
				t.Errorf("Select(%q) = %q, want %q", tc.path, got.PathPattern, tc.want.PathPattern)
			}
		})
	}
}

func TestSelect_NoBehaviorsReturnsDefault(t *testing.T) {
	def := &config.Behavior{}
	dist := &config.Distribution{DefaultBehavior: def}
	if got := Select(dist, "/anything"); got != def {
		t.Errorf("Select with no cache behaviors should return the default behavior")
	}
}

func TestSynthesizeViewerHeaders_ViewerAddressKeepsPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.RemoteAddr = "203.0.113.5:4444"
	h := SynthesizeViewerHeaders(r)
	// CloudFront-Viewer-Address carries the full "ip:port", not the IP alone.
	if got := h.Get("CloudFront-Viewer-Address"); got != "203.0.113.5:4444" {
		t.Errorf("CloudFront-Viewer-Address = %q, want 203.0.113.5:4444", got)
	}
}

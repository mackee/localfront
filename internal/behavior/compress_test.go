package behavior

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/mackee/localfront/internal/config"
)

func respHeader(contentType string, length int, extra map[string]string) http.Header {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if length >= 0 {
		h.Set("Content-Length", strconv.Itoa(length))
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

func TestChooseEncoding(t *testing.T) {
	tests := []struct {
		name           string
		compress       bool
		acceptEncoding string
		contentType    string
		length         int
		extra          map[string]string
		want           string
	}{
		{"compress off", false, "gzip, br", "text/html", 5000, nil, ""},
		{"gzip preferred over br", true, "br, gzip", "text/html", 5000, nil, "gzip"},
		{"br when only br accepted", true, "br", "application/json", 5000, nil, "br"},
		{"no acceptable encoding", true, "identity", "text/html", 5000, nil, ""},
		{"non-compressible type", true, "gzip", "image/png", 5000, nil, ""},
		{"below size threshold", true, "gzip", "text/html", 999, nil, ""},
		{"above size threshold", true, "gzip", "text/html", 10_000_001, nil, ""},
		{"at lower bound", true, "gzip", "text/html", 1000, nil, "gzip"},
		{"missing content-length", true, "gzip", "text/html", -1, nil, ""},
		{"already encoded", true, "gzip", "text/html", 5000, map[string]string{"Content-Encoding": "gzip"}, ""},
		{"no-transform blocks compression", true, "gzip", "text/html", 5000, map[string]string{"Cache-Control": "public, no-transform"}, ""},
		{"content type with charset", true, "gzip", "text/html; charset=utf-8", 5000, nil, "gzip"},
		{"javascript compresses", true, "gzip", "application/javascript", 2000, nil, "gzip"},
		{"svg compresses", true, "gzip", "image/svg+xml", 2000, nil, "gzip"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &config.Behavior{Compress: tc.compress}
			got := ChooseEncoding(b, tc.acceptEncoding, respHeader(tc.contentType, tc.length, tc.extra))
			if got != tc.want {
				t.Errorf("ChooseEncoding = %q, want %q", got, tc.want)
			}
		})
	}
}

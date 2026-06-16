package behavior

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/mackee/localfront/internal/config"
)

// Compression bounds, matching CloudFront's documented thresholds: objects are
// only compressed when their length is known and falls within this range.
const (
	minCompressBytes = 1000
	maxCompressBytes = 10_000_000
)

// compressibleTypes is CloudFront's documented set of compressible content
// types, matched against the media type (the part before any ";").
var compressibleTypes = map[string]bool{
	"application/dash+xml":               true,
	"application/eot":                    true,
	"application/font":                   true,
	"application/font-sfnt":              true,
	"application/javascript":             true,
	"application/json":                   true,
	"application/opentype":               true,
	"application/otf":                    true,
	"application/pdf":                    true,
	"application/pkcs7-mime":             true,
	"application/protobuf":               true,
	"application/rss+xml":                true,
	"application/truetype":               true,
	"application/ttf":                    true,
	"application/vnd.apple.mpegurl":      true,
	"application/vnd.mapbox-vector-tile": true,
	"application/vnd.ms-fontobject":      true,
	"application/wasm":                   true,
	"application/xhtml+xml":              true,
	"application/xml":                    true,
	"application/x-font-opentype":        true,
	"application/x-font-truetype":        true,
	"application/x-font-ttf":             true,
	"application/x-httpd-cgi":            true,
	"application/x-javascript":           true,
	"application/x-mpegurl":              true,
	"application/x-opentype":             true,
	"application/x-otf":                  true,
	"application/x-perl":                 true,
	"application/x-ttf":                  true,
	"font/eot":                           true,
	"font/opentype":                      true,
	"font/otf":                           true,
	"font/ttf":                           true,
	"image/svg+xml":                      true,
	"text/css":                           true,
	"text/csv":                           true,
	"text/html":                          true,
	"text/javascript":                    true,
	"text/js":                            true,
	"text/plain":                         true,
	"text/richtext":                      true,
	"text/tab-separated-values":          true,
	"text/xml":                           true,
	"text/x-component":                   true,
	"text/x-java-source":                 true,
	"text/x-script":                      true,
	"vnd.apple.mpegurl":                  true,
}

// ChooseEncoding decides whether and how to compress an origin response on the
// fly. It returns "gzip", "br", or "" (no compression), reproducing
// CloudFront's rules: the behavior must enable compression, the viewer must
// accept the encoding, the response must be a compressible content type of
// known length within bounds, must not already be encoded, and must not carry
// Cache-Control: no-transform. gzip is preferred over Brotli for determinism.
func ChooseEncoding(b *config.Behavior, acceptEncoding string, respHeader http.Header) string {
	if !b.Compress {
		return ""
	}
	if respHeader.Get("Content-Encoding") != "" {
		return ""
	}
	if !compressibleTypes[mediaType(respHeader.Get("Content-Type"))] {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(respHeader.Get("Content-Length")))
	if err != nil || n < minCompressBytes || n > maxCompressBytes {
		return ""
	}
	if hasNoTransform(respHeader.Get("Cache-Control")) {
		return ""
	}
	accepts := parseAcceptEncoding(acceptEncoding)
	switch {
	case accepts["gzip"]:
		return "gzip"
	case accepts["br"]:
		return "br"
	default:
		return ""
	}
}

func mediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

func hasNoTransform(cacheControl string) bool {
	for _, directive := range strings.Split(cacheControl, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
			return true
		}
	}
	return false
}

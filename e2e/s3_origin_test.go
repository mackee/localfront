//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// The spa-hosting and static-and-api examples are verified with docker-compose
// + runn (see Taskfile `verify:examples`). This file keeps the finer-grained
// S3 scenario coverage that is not tied to an example.

// TestMediaDelivery covers Range and conditional requests against an S3 origin
// (scenario 5): partial content and 304 Not Modified.
func TestMediaDelivery(t *testing.T) {
	st := startStore(t)
	const payload = "0123456789abcdef" // 16 bytes, deterministic
	st.put(t, "media", "", "", nil)
	st.put(t, "media", "clip.txt", "text/plain", []byte(payload))
	lf := startLocalfront(t, repoFile(t, "e2e/testdata/media.template.yaml"), st)

	const host = "media.example.test"

	t.Run("range returns 206 and exact bytes", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host, "/clip.txt", map[string]string{"Range": "bytes=0-4"})
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("ranged GET = %d, want 206\n%s", resp.StatusCode, fmtHeaders(resp.Header))
		}
		if string(body) != "01234" {
			t.Errorf("range body = %q, want %q", body, "01234")
		}
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-4/") {
			t.Errorf("Content-Range = %q, want bytes 0-4/...", cr)
		}
	})

	t.Run("matching ETag returns 304", func(t *testing.T) {
		first := lf.get(t, http.MethodGet, host, "/clip.txt", nil)
		_ = mustReadBody(t, first)
		etag := first.Header.Get("ETag")
		if etag == "" {
			t.Fatal("origin did not return an ETag")
		}
		resp := lf.get(t, http.MethodGet, host, "/clip.txt", map[string]string{"If-None-Match": etag})
		_ = mustReadBody(t, resp)
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional GET = %d, want 304", resp.StatusCode)
		}
	})
}

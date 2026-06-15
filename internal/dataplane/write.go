package dataplane

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"

	"github.com/andybalholm/brotli"
	"github.com/mackee/localfront/internal/behavior"
	"github.com/mackee/localfront/internal/config"
)

// writeResponse finalizes an origin response: it applies on-the-fly
// compression when the behavior calls for it, sets the CloudFront response
// headers, and streams the body to the viewer.
func (s *Server) writeResponse(w http.ResponseWriter, r *http.Request, dist *config.Distribution, beh *config.Behavior, resp *originResponse, requestID string) {
	out := w.Header()
	copyHeaders(out, resp.header)
	removeHopByHopHeaders(out)
	addVia(out, dist.DomainName)
	out.Set("X-Cache", cacheStatus(resp.statusCode))
	out.Set("X-Amz-Cf-Id", requestID)
	out.Set("X-Amz-Cf-Pop", popName)

	encoding := ""
	if resp.statusCode == http.StatusOK && r.Method != http.MethodHead {
		encoding = behavior.ChooseEncoding(beh, r.Header.Get("Accept-Encoding"), resp.header)
	}
	if encoding == "" {
		w.WriteHeader(resp.statusCode)
		if r.Method != http.MethodHead {
			if _, err := io.Copy(w, resp.body); err != nil {
				s.logger.Debug("copying origin response failed", "error", err)
			}
		}
		return
	}

	compressed, err := compressBody(encoding, resp.body)
	if err != nil {
		// Fall back to the uncompressed body on a compression failure.
		s.logger.Debug("compressing response failed; sending uncompressed", "error", err)
		w.WriteHeader(resp.statusCode)
		if _, copyErr := io.Copy(w, resp.body); copyErr != nil {
			s.logger.Debug("copying origin response failed", "error", copyErr)
		}
		return
	}
	out.Set("Content-Encoding", encoding)
	out.Set("Content-Length", strconv.Itoa(len(compressed)))
	addResponseVary(out, "Accept-Encoding")
	w.WriteHeader(resp.statusCode)
	if _, err := w.Write(compressed); err != nil {
		s.logger.Debug("writing compressed response failed", "error", err)
	}
}

// compressBody reads body fully and compresses it. The body length is bounded
// by behavior.ChooseEncoding to CloudFront's 10 MB compression ceiling, so
// buffering it is safe.
func compressBody(encoding string, body io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	var wc io.WriteCloser
	switch encoding {
	case "gzip":
		wc = gzip.NewWriter(&buf)
	case "br":
		wc = brotli.NewWriter(&buf)
	default:
		return nil, io.ErrNoProgress
	}
	if _, err := io.Copy(wc, body); err != nil {
		_ = wc.Close()
		return nil, err
	}
	if err := wc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cacheStatus(status int) string {
	if status >= 400 {
		return "Error from localfront"
	}
	return "Miss from localfront"
}

func addResponseVary(h http.Header, value string) {
	for _, existing := range h.Values("Vary") {
		if existing == value {
			return
		}
	}
	h.Add("Vary", value)
}

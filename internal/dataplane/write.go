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

	raw, err := io.ReadAll(resp.body)
	if err != nil {
		s.logger.Debug("reading origin response failed", "error", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	compressed, err := compressBytes(encoding, raw)
	if err != nil {
		s.logger.Debug("compressing response failed; sending uncompressed", "error", err)
		out.Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(resp.statusCode)
		if _, err := w.Write(raw); err != nil {
			s.logger.Debug("writing uncompressed response failed", "error", err)
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

// compressBytes compresses an already-buffered body. CloudFront's 10 MB
// compression ceiling (enforced via behavior.ChooseEncoding) makes buffering
// safe.
func compressBytes(encoding string, raw []byte) ([]byte, error) {
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
	if _, err := wc.Write(raw); err != nil {
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

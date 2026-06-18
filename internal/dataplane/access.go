package dataplane

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mackee/localfront/internal/accesslog"
	"github.com/mackee/localfront/internal/config"
)

// accessRecorder wraps an http.ResponseWriter so the server can recover the
// status, byte counts, and first-byte time it would otherwise see only at the
// wire level. Without a writer, every method is a passthrough.
type accessRecorder struct {
	http.ResponseWriter
	status            int
	wroteHeader       bool
	headerBytes       int64
	bodyBytes         int64
	firstByteAt       time.Time
	requestStart      time.Time
	protoVersion      string
	requestBytes      *countingReadCloser
	wroteFirstAt      bool
	functionGenerated bool
	now               func() time.Time
}

// markFunctionGenerated records that a CloudFront Function short-circuited the
// pipeline with its own response. The access log surfaces that distinction in
// the three result-type columns so downstream tooling can attribute the reply
// to the function rather than to the origin or the cache.
func (r *accessRecorder) markFunctionGenerated() {
	if r != nil {
		r.functionGenerated = true
	}
}

// newAccessRecorder wraps w so ServeHTTP can read back the response bytes,
// status, and first-byte time at the end of the request. now is the clock used
// for the first-byte timestamp (the same one the server times start/end with);
// a nil now falls back to time.Now.
func newAccessRecorder(w http.ResponseWriter, start time.Time, protoVersion string, now func() time.Time) *accessRecorder {
	if now == nil {
		now = time.Now
	}
	return &accessRecorder{
		ResponseWriter: w,
		requestStart:   start,
		protoVersion:   protoVersion,
		now:            now,
	}
}

// Flush forwards to the embedded writer's Flush when supported. CloudFront
// Functions and compressed responses do not flush mid-stream in localfront,
// but origins or future features may, so keep the interface intact.
func (r *accessRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *accessRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		r.ResponseWriter.WriteHeader(status)
		return
	}
	r.wroteHeader = true
	r.status = status
	r.headerBytes = estimateResponseHeaderBytes(status, r.protoVersion, r.Header())
	if !r.wroteFirstAt {
		r.firstByteAt = r.now()
		r.wroteFirstAt = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *accessRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if !r.wroteFirstAt {
		r.firstByteAt = r.now()
		r.wroteFirstAt = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bodyBytes += int64(n)
	return n, err
}

// estimateResponseHeaderBytes approximates the wire byte count of the response
// status line and header block. Go's net/http does not expose the serialized
// size, so localfront reports the textual length of HTTP/1.1's framing, which
// stays within a handful of bytes of what the connection actually sent. This
// matches what real CloudFront ETL consumers care about (order of magnitude).
func estimateResponseHeaderBytes(status int, protoVersion string, h http.Header) int64 {
	proto := protoVersion
	if proto == "" {
		proto = "HTTP/1.1"
	}
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "Status"
	}
	// "HTTP/1.1 200 OK\r\n"
	line := proto + " " + strconv.Itoa(status) + " " + statusText + "\r\n"
	n := int64(len(line))
	for k, vs := range h {
		for _, v := range vs {
			n += int64(len(k)) + 2 + int64(len(v)) + 2 // "Key: value\r\n"
		}
	}
	n += 2 // final CRLF
	return n
}

// estimateRequestHeaderBytes approximates the wire size of the request line
// and headers, mirroring estimateResponseHeaderBytes.
func estimateRequestHeaderBytes(r *http.Request) int64 {
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	proto := r.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	line := r.Method + " " + uri + " " + proto + "\r\n"
	n := int64(len(line))
	if r.Host != "" {
		n += int64(len("Host: ")) + int64(len(r.Host)) + 2
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			n += int64(len(k)) + 2 + int64(len(v)) + 2
		}
	}
	n += 2
	return n
}

// countingReadCloser counts the bytes read from r.Body so cs-bytes includes
// the request body when Content-Length is unknown (chunked uploads).
type countingReadCloser struct {
	io.ReadCloser
	read int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.read += int64(n)
	return n, err
}

func (r *accessRecorder) wrapRequestBody(req *http.Request) {
	if req.Body == nil || req.Body == http.NoBody {
		return
	}
	r.requestBytes = &countingReadCloser{ReadCloser: req.Body}
	req.Body = r.requestBytes
}

// observedStatus reports the status written to the wire, defaulting to 200
// (Go's net/http does the same when no WriteHeader call ever lands).
func (r *accessRecorder) observedStatus() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// buildAccessEntry assembles an *accesslog.Entry from the captured wire data
// and the per-request metadata. dist may be nil when no distribution matched
// the Host header (the unknown-host 403 case).
func buildAccessEntry(r *http.Request, rec *accessRecorder, dist *config.Distribution, requestID string, end time.Time) *accesslog.Entry {
	host := r.Host
	cs := strings.ToLower(hostOnly(host))
	cip, cport := splitRemote(r.RemoteAddr)

	resultType := edgeResultType(rec.observedStatus(), rec.functionGenerated)

	stem := r.URL.Path
	if r.URL.RawPath != "" {
		stem = r.URL.RawPath
	}

	reqBytes := estimateRequestHeaderBytes(r)
	if rec.requestBytes != nil {
		reqBytes += rec.requestBytes.read
	} else if r.ContentLength > 0 {
		reqBytes += r.ContentLength
	}

	timeTaken := end.Sub(rec.requestStart)
	var ttfb time.Duration
	if rec.wroteFirstAt {
		ttfb = rec.firstByteAt.Sub(rec.requestStart)
	} else {
		ttfb = timeTaken
	}

	respHeader := rec.Header()
	contentType := respHeader.Get("Content-Type")
	contentLen := respHeader.Get("Content-Length")
	if contentLen == "" && rec.bodyBytes > 0 {
		contentLen = strconv.FormatInt(rec.bodyBytes, 10)
	}
	rangeStart, rangeEnd := parseContentRange(respHeader.Get("Content-Range"))

	return &accesslog.Entry{
		Time:                   end.UTC(),
		EdgeLocation:           popName,
		SCBytes:                rec.headerBytes + rec.bodyBytes,
		CIP:                    cip,
		CSMethod:               r.Method,
		CSHost:                 cs,
		CSURIStem:              stem,
		SCStatus:               rec.observedStatus(),
		CSReferer:              r.Header.Get("Referer"),
		CSUserAgent:            r.Header.Get("User-Agent"),
		CSURIQuery:             r.URL.RawQuery,
		CSCookie:               r.Header.Get("Cookie"),
		EdgeResultType:         resultType,
		EdgeRequestID:          requestID,
		XHostHeader:            host,
		CSProtocol:             "http",
		CSBytes:                reqBytes,
		TimeTaken:              timeTaken,
		XForwardedFor:          r.Header.Get("X-Forwarded-For"),
		SSLProtocol:            "",
		SSLCipher:              "",
		EdgeResponseResultType: resultType,
		CSProtocolVersion:      protoOrDefault(r.Proto),
		FLEStatus:              "",
		FLEEncryptedFields:     "",
		CPort:                  cport,
		TimeToFirstByte:        ttfb,
		EdgeDetailedResultType: resultType,
		SCContentType:          contentType,
		SCContentLen:           contentLen,
		SCRangeStart:           rangeStart,
		SCRangeEnd:             rangeEnd,
	}
}

func protoOrDefault(p string) string {
	if p == "" {
		return "HTTP/1.1"
	}
	return p
}

// edgeResultType maps a request outcome to the CloudFront result-type column.
// The PoC has no cache, so non-error responses are always "Miss". A response
// produced by a CloudFront Function short-circuit is logged as
// "FunctionGeneratedResponse" regardless of status, which is the value real
// CloudFront emits in the detailed-result-type column for that case.
func edgeResultType(status int, functionGenerated bool) string {
	if functionGenerated {
		return "FunctionGeneratedResponse"
	}
	if status >= 400 {
		return "Error"
	}
	return "Miss"
}

func splitRemote(addr string) (string, int) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

// parseContentRange splits a Content-Range header into the (sc-range-start,
// sc-range-end) byte offsets reported in the access log, returning empty
// strings for any value that does not look like "bytes start-end/total".
func parseContentRange(v string) (string, string) {
	const prefix = "bytes "
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, prefix) {
		return "", ""
	}
	v = strings.TrimPrefix(v, prefix)
	slash := strings.IndexByte(v, '/')
	if slash >= 0 {
		v = v[:slash]
	}
	dash := strings.IndexByte(v, '-')
	if dash <= 0 || dash == len(v)-1 {
		return "", ""
	}
	return v[:dash], v[dash+1:]
}

// emitAccessLog writes one Standard-log line for a completed request. It is
// a no-op when access logging is disabled.
func (s *Server) emitAccessLog(r *http.Request, rec *accessRecorder, dist *config.Distribution, requestID string) {
	if s.accessLog == nil || rec == nil {
		return
	}
	entry := buildAccessEntry(r, rec, dist, requestID, s.now())
	s.accessLog.Write(entry)
}


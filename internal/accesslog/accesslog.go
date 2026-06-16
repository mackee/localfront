// Package accesslog emits per-request access logs in CloudFront's Standard
// log format (tab-separated, 33 columns, with the W3C-style `#Version:` and
// `#Fields:` header). The format and column order match the layout CloudFront
// delivers to S3, so downstream ETL/analysis tooling can consume localfront
// output without changes.
//
// Real-time logs (the configurable, Kinesis-shaped variant) are not produced
// here. Fields not meaningful to localfront's PoC — TLS attributes, FLE,
// content-range — are emitted as the standard placeholder "-".
package accesslog

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Header is the two-line W3C-style preamble CloudFront writes at the top of
// every Standard log file. It is emitted once when the writer is created.
const Header = "#Version: 1.0\n" +
	"#Fields: date time x-edge-location sc-bytes c-ip cs-method cs(Host) " +
	"cs-uri-stem sc-status cs(Referer) cs(User-Agent) cs-uri-query cs(Cookie) " +
	"x-edge-result-type x-edge-request-id x-host-header cs-protocol cs-bytes " +
	"time-taken x-forwarded-for ssl-protocol ssl-cipher x-edge-response-result-type " +
	"cs-protocol-version fle-status fle-encrypted-fields c-port time-to-first-byte " +
	"x-edge-detailed-result-type sc-content-type sc-content-len sc-range-start " +
	"sc-range-end\n"

// Entry holds the values for one log line. The writer renders them in the
// fixed CloudFront column order. Empty strings render as the "-" placeholder.
type Entry struct {
	Time                   time.Time
	EdgeLocation           string
	SCBytes                int64
	CIP                    string
	CSMethod               string
	CSHost                 string
	CSURIStem              string
	SCStatus               int
	CSReferer              string
	CSUserAgent            string
	CSURIQuery             string
	CSCookie               string
	EdgeResultType         string
	EdgeRequestID          string
	XHostHeader            string
	CSProtocol             string
	CSBytes                int64
	TimeTaken              time.Duration
	XForwardedFor          string
	SSLProtocol            string
	SSLCipher              string
	EdgeResponseResultType string
	CSProtocolVersion      string
	FLEStatus              string
	FLEEncryptedFields     string
	CPort                  int
	TimeToFirstByte        time.Duration
	EdgeDetailedResultType string
	SCContentType          string
	SCContentLen           string
	SCRangeStart           string
	SCRangeEnd             string
}

// Writer renders Entry values into a destination writer. It is safe to call
// Write from multiple goroutines; each entry reaches the destination as a
// single, complete line.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter returns a Writer that emits log lines into dst. The Standard-log
// header is written immediately so partial files are still readable.
func NewWriter(dst io.Writer) (*Writer, error) {
	w := &Writer{w: dst}
	if _, err := io.WriteString(dst, Header); err != nil {
		return nil, fmt.Errorf("writing access log header: %w", err)
	}
	return w, nil
}

// Write renders e into the destination. It serializes writes and remembers
// the first I/O error so callers can surface it at shutdown.
func (w *Writer) Write(e *Entry) {
	if w == nil {
		return
	}
	line := renderEntry(e)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if _, err := io.WriteString(w.w, line); err != nil {
		w.err = err
	}
}

// Err returns the first I/O error the writer observed, or nil. The writer
// stops emitting after the first error so a broken stdout does not panic the
// request goroutine on every subsequent log line.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// renderEntry formats one Entry as a single tab-separated line followed by a
// newline.
func renderEntry(e *Entry) string {
	utc := e.Time.UTC()
	date := utc.Format("2006-01-02")
	clock := utc.Format("15:04:05")
	fields := []string{
		date,
		clock,
		fieldOrDash(e.EdgeLocation),
		intField(e.SCBytes),
		fieldOrDash(e.CIP),
		fieldOrDash(e.CSMethod),
		fieldOrDash(e.CSHost),
		fieldOrDash(e.CSURIStem),
		statusField(e.SCStatus),
		fieldOrDash(e.CSReferer),
		fieldOrDash(e.CSUserAgent),
		fieldOrDash(e.CSURIQuery),
		fieldOrDash(e.CSCookie),
		fieldOrDash(e.EdgeResultType),
		fieldOrDash(e.EdgeRequestID),
		fieldOrDash(e.XHostHeader),
		fieldOrDash(e.CSProtocol),
		intField(e.CSBytes),
		durationField(e.TimeTaken),
		fieldOrDash(e.XForwardedFor),
		fieldOrDash(e.SSLProtocol),
		fieldOrDash(e.SSLCipher),
		fieldOrDash(e.EdgeResponseResultType),
		fieldOrDash(e.CSProtocolVersion),
		fieldOrDash(e.FLEStatus),
		fieldOrDash(e.FLEEncryptedFields),
		intField(int64(e.CPort)),
		durationField(e.TimeToFirstByte),
		fieldOrDash(e.EdgeDetailedResultType),
		fieldOrDash(e.SCContentType),
		fieldOrDash(e.SCContentLen),
		fieldOrDash(e.SCRangeStart),
		fieldOrDash(e.SCRangeEnd),
	}
	return strings.Join(fields, "\t") + "\n"
}

// fieldOrDash URL-encodes a value so tabs, spaces, and other control bytes
// can't break the tab-separated layout. The empty string becomes "-", which
// matches the placeholder CloudFront uses for absent values.
func fieldOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return encodeField(s)
}

// statusField formats the response status code. CloudFront writes "000" when
// the viewer disconnected before any status was sent.
func statusField(status int) string {
	if status <= 0 {
		return "000"
	}
	return fmt.Sprintf("%d", status)
}

// intField formats a non-negative byte/port counter, "0" when the counter is
// zero (CloudFront does not substitute "-" for numeric zero).
func intField(n int64) string {
	if n < 0 {
		return "0"
	}
	return fmt.Sprintf("%d", n)
}

// durationField formats a duration in seconds with three decimal places,
// which matches CloudFront's millisecond resolution.
func durationField(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.3f", d.Seconds())
}

// encodeField percent-encodes the bytes CloudFront documents as escaped in
// access log values: whitespace, control bytes, and the field-delimiter byte
// (tab). Everything else passes through.
func encodeField(s string) string {
	needs := false
	for i := range len(s) {
		if mustEscape(s[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	b := make([]byte, 0, len(s)+8)
	for i := range len(s) {
		c := s[i]
		if mustEscape(c) {
			b = append(b, '%', hexDigit(c>>4), hexDigit(c&0x0f))
			continue
		}
		b = append(b, c)
	}
	return string(b)
}

func mustEscape(c byte) bool {
	if c <= 0x20 || c == 0x7f {
		return true
	}
	if c == '%' {
		return true
	}
	return false
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'A' + (n - 10)
}

package accesslog

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewWriterEmitsHeader(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWriter(&buf); err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "#Version: 1.0\n#Fields: ") {
		t.Fatalf("missing W3C header prefix; got %q", got)
	}
	if !strings.HasSuffix(got, " sc-range-end\n") {
		t.Fatalf("missing final field name in header; got %q", got)
	}
	// 33 field names plus the two prefix tokens "#Fields:" and the "date".
	fieldsLine := strings.TrimPrefix(strings.Split(got, "\n")[1], "#Fields: ")
	if got, want := len(strings.Fields(fieldsLine)), 33; got != want {
		t.Fatalf("expected 33 field names, got %d in %q", got, fieldsLine)
	}
}

func TestWriteRendersOneTabSeparatedLine(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Write(&Entry{
		Time:              time.Date(2026, 6, 16, 12, 34, 56, 0, time.UTC),
		EdgeLocation:      "LOCAL50-C1",
		SCBytes:           1234,
		CIP:               "127.0.0.1",
		CSMethod:          "GET",
		CSHost:            "assets.example.test",
		CSURIStem:         "/index.html",
		SCStatus:          200,
		CSReferer:         "",
		CSUserAgent:       "curl/8.0",
		CSURIQuery:        "",
		EdgeResultType:    "Miss",
		EdgeRequestID:     "abc",
		XHostHeader:       "assets.example.test",
		CSProtocol:        "http",
		CSBytes:           250,
		TimeTaken:         12 * time.Millisecond,
		CSProtocolVersion: "HTTP/1.1",
		CPort:             54321,
		TimeToFirstByte:   8 * time.Millisecond,
		SCContentType:     "text/html",
		SCContentLen:      "1024",
	})
	body := strings.TrimPrefix(buf.String(), Header)
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("missing trailing newline in line: %q", body)
	}
	cols := strings.Split(strings.TrimRight(body, "\n"), "\t")
	if got, want := len(cols), 33; got != want {
		t.Fatalf("expected 33 columns, got %d: %#v", got, cols)
	}
	checks := []struct {
		idx  int
		want string
	}{
		{0, "2026-06-16"},
		{1, "12:34:56"},
		{2, "LOCAL50-C1"},
		{3, "1234"},
		{4, "127.0.0.1"},
		{5, "GET"},
		{6, "assets.example.test"},
		{7, "/index.html"},
		{8, "200"},
		{9, "-"},
		{10, "curl/8.0"},
		{11, "-"},
		{12, "-"},
		{13, "Miss"},
		{14, "abc"},
		{15, "assets.example.test"},
		{16, "http"},
		{17, "250"},
		{18, "0.012"},
		{19, "-"},
		{20, "-"},
		{21, "-"},
		{22, "-"},
		{23, "HTTP/1.1"},
		{24, "-"},
		{25, "-"},
		{26, "54321"},
		{27, "0.008"},
		{28, "-"},
		{29, "text/html"},
		{30, "1024"},
		{31, "-"},
		{32, "-"},
	}
	for _, c := range checks {
		if cols[c.idx] != c.want {
			t.Errorf("col %d: got %q, want %q", c.idx, cols[c.idx], c.want)
		}
	}
}

func TestStatusZeroIsTripleZero(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Write(&Entry{Time: time.Unix(0, 0).UTC(), SCStatus: 0})
	body := strings.TrimPrefix(buf.String(), Header)
	cols := strings.Split(strings.TrimRight(body, "\n"), "\t")
	if cols[8] != "000" {
		t.Fatalf("sc-status: want 000, got %q", cols[8])
	}
}

func TestEncodeFieldEscapesSpacesAndTabs(t *testing.T) {
	cases := map[string]string{
		"":                "-",
		"plain":           "plain",
		"a b":             "a%20b",
		"a\tb":            "a%09b",
		"line\nbreak":     "line%0Abreak",
		"100% sure":       "100%25%20sure",
		"path/with?key=v": "path/with?key=v",
	}
	for in, want := range cases {
		if got := fieldOrDash(in); got != want {
			t.Errorf("fieldOrDash(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestWriteIsGoroutineSafe(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	const goroutines = 16
	const perGoroutine = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				w.Write(&Entry{
					Time:     time.Unix(0, 0).UTC(),
					SCStatus: 200,
					CSMethod: "GET",
				})
			}
		}()
	}
	wg.Wait()
	body := strings.TrimPrefix(buf.String(), Header)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if got, want := len(lines), goroutines*perGoroutine; got != want {
		t.Fatalf("line count: got %d, want %d", got, want)
	}
	for i, l := range lines {
		cols := strings.Split(l, "\t")
		if len(cols) != 33 {
			t.Fatalf("line %d: expected 33 columns, got %d", i, len(cols))
		}
	}
}

type failingWriter struct{ count int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.count++
	return 0, io.ErrShortWrite
}

func TestWriteRecordsFirstError(t *testing.T) {
	// NewWriter writes the header; use a happy path then swap in a failing
	// destination — but the public API is sealed, so exercise Err() via the
	// constructor instead.
	w, err := NewWriter(io.Discard)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if w.Err() != nil {
		t.Fatalf("Err() should be nil initially, got %v", w.Err())
	}
	// Force the failing path: replace the embedded writer (white-box).
	w.w = &failingWriter{}
	w.Write(&Entry{Time: time.Unix(0, 0).UTC()})
	if w.Err() == nil {
		t.Fatalf("Err() should report the first I/O failure")
	}
	// Subsequent writes are suppressed.
	fw := w.w.(*failingWriter)
	before := fw.count
	w.Write(&Entry{Time: time.Unix(0, 0).UTC()})
	if fw.count != before {
		t.Fatalf("expected writes to stop after first error: count went %d -> %d", before, fw.count)
	}
}

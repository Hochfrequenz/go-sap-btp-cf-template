package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// clguard_test.go unit-tests contentLengthGuard directly (no compress()
// wrapper), so a failure here points straight at the guard rather than
// at its interaction with gzhttp. Test_newHTTPServer_TruncatedHandler_*
// in server_test.go covers the composed compress(contentLengthGuard(r))
// wiring end to end; these tests are the narrower, faster complement.
//
// contentLengthGuard's contract: it panics with http.ErrAbortHandler
// when a handler declares a Content-Length and then writes fewer bytes
// than that before returning. net/http's own server recovers that panic
// and aborts the connection without a clean terminator, so from an
// httptest.Server's client the effect is a request error or a body read
// error — never a successful read of a short body.

// doThroughGuard runs one request through contentLengthGuard(handler) via
// a real httptest.Server (so net/http's panic recovery is exercised, not
// just the ResponseWriter in isolation), returning either a completed
// response or the error from doing the request/reading its body.
func doThroughGuard(t *testing.T, h http.HandlerFunc, method string) (*http.Response, []byte, error) {
	t.Helper()
	ts := httptest.NewServer(contentLengthGuard(h))
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(method, ts.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func Test_contentLengthGuard_TruncatedWrite_Aborts(t *testing.T) {
	full := strings.Repeat("y", 200)
	half := full[:len(full)/2]
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, half)
	})

	_, _, err := doThroughGuard(t, h, http.MethodGet)
	if err == nil {
		t.Fatal("expected a request/read error for a truncated write, got none")
	}
}

func Test_contentLengthGuard_FullWrite_Passes(t *testing.T) {
	full := strings.Repeat("z", 200)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, full)
	})

	resp, body, err := doThroughGuard(t, h, http.MethodGet)
	if err != nil {
		t.Fatalf("unexpected error for a full write: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != full {
		t.Fatalf("body = %q (%d bytes), want %d bytes", body, len(body), len(full))
	}
}

func Test_contentLengthGuard_NoContentLength_Passes(t *testing.T) {
	// A handler that never sets Content-Length (chunked transfer) has no
	// "want" for the guard to compare against (want stays -1), so it
	// must never be flagged as truncated regardless of how little it
	// writes.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
	})

	resp, body, err := doThroughGuard(t, h, http.MethodGet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "short" {
		t.Fatalf("body = %q, want %q", body, "short")
	}
}

func Test_contentLengthGuard_304_NoBodyExpected_Passes(t *testing.T) {
	// A 304 has no body by definition (RFC 9110 §15.4.5); a
	// Content-Length echoed alongside it (as caches commonly do,
	// mirroring the resource's real size) must not be interpreted as a
	// body the handler owed but didn't write.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusNotModified)
	})

	resp, body, err := doThroughGuard(t, h, http.MethodGet)
	if err != nil {
		t.Fatalf("unexpected error for 304: %v", err)
	}
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("body = %d bytes, want 0 for 304", len(body))
	}
}

func Test_contentLengthGuard_HEAD_NoBodyExpected_Passes(t *testing.T) {
	// RFC 9110 §9.3.2: a HEAD response carries the Content-Length a GET
	// would, but the handler never writes a body. "written < want" is
	// the expected, correct shape for HEAD, not a truncation.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "999")
		w.WriteHeader(http.StatusOK)
		// deliberately no body write, matching how net/http itself
		// suppresses HEAD bodies
	})

	resp, _, err := doThroughGuard(t, h, http.MethodHead)
	if err != nil {
		t.Fatalf("unexpected error for HEAD: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func Test_contentLengthGuard_Informational1xx_DoesNotLatch(t *testing.T) {
	// A handler that sends a 103 Early Hints before its real 200 must
	// not have the guard latch onto the 103 as "the" WriteHeader call:
	// that would freeze wroteHeader before the real Content-Length (set
	// only on the follow-up 200) is ever read.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)

		full := strings.Repeat("q", 50)
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, full)
	})

	resp, body, err := doThroughGuard(t, h, http.MethodGet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (final status, not the 103)", resp.StatusCode)
	}
	want := strings.Repeat("q", 50)
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func Test_contentLengthGuard_FlushSSE_Unaffected(t *testing.T) {
	// A streaming/SSE-style handler that never sets Content-Length
	// (want stays -1) and flushes incrementally must be passed through
	// untouched: Flush must reach the real ResponseWriter, and the full
	// streamed body must still arrive.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			panic("ResponseWriter passed to handler does not implement http.Flusher")
		}
		for range 3 {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			flusher.Flush()
		}
	})

	ts := httptest.NewServer(contentLengthGuard(h))
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	chunks := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			chunks++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if chunks != 3 {
		t.Fatalf("got %d SSE chunks, want 3", chunks)
	}
}

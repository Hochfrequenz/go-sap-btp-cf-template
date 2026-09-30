package main

import (
	"bufio"
	"context"
	"errors"
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
// when a handler declares a Content-Length and then writes a different
// number of bytes than that before returning — fewer (short) or more
// (overlong). net/http's own server recovers that panic and aborts the
// connection without a clean terminator, so from an httptest.Server's
// client the effect is a request error or a body read error — never a
// successful read of a short or overlong body.

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

// Test_clGuardWriter_Write_NeverForwardsBytesPastDeclaredLength exercises
// clGuardWriter.Write directly (no contentLengthGuard, no panic) to prove
// the clamp itself: an overlong call must never forward any of its bytes
// to the underlying writer, not just some of them. This is what stops
// gzhttp — sitting below this writer in production — from ever seeing,
// let alone framing, the excess bytes, independent of whether the
// end-of-handler abort in contentLengthGuard also fires. Without this
// clamp, a large enough overlong write could already be flushed onto the
// wire by the time the handler returns and the guard panics, at which
// point the panic can no longer take the leaked bytes back.
func Test_clGuardWriter_Write_NeverForwardsBytesPastDeclaredLength(t *testing.T) {
	rec := httptest.NewRecorder()
	g := &clGuardWriter{ResponseWriter: rec, want: -1, ctx: context.Background()}
	g.Header().Set("Content-Length", "100")
	g.WriteHeader(http.StatusOK)

	n, err := g.Write([]byte(strings.Repeat("z", 250)))
	if !errors.Is(err, http.ErrContentLength) {
		t.Fatalf("err = %v, want http.ErrContentLength", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 for a call that overflows on its own (matches net/http's response.write, which forwards none of an overflowing call)", n)
	}
	if got := rec.Body.Len(); got > 100 {
		t.Fatalf("clGuardWriter forwarded %d bytes to the underlying writer, want at most the declared 100 — an overlong write must never reach whatever sits below (e.g. gzhttp), even partially", got)
	}
}

// guardAborts runs h through contentLengthGuard alone and reports whether
// the guard raised http.ErrAbortHandler. The httptest.Server-based tests
// above cannot tell: without gzhttp in front, net/http enforces
// Content-Length itself and breaks the connection even with no guard at
// all, so they pass with the guard deleted.
func guardAborts(t *testing.T, h http.Handler, method string) (aborted bool) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			if rec != http.ErrAbortHandler {
				panic(rec)
			}
			aborted = true
		}
	}()
	contentLengthGuard(h).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/", nil))
	return false
}

func Test_contentLengthGuard_AbortDecision(t *testing.T) {
	cases := []struct {
		name   string
		method string
		h      http.HandlerFunc
		want   bool
	}{
		{"explicit 200, short", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, strings.Repeat("y", 100))
		}, true},
		{"implicit 200, short", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "200")
			_, _ = io.WriteString(w, strings.Repeat("y", 100))
		}, true},
		{"explicit 200, overlong", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			// A handler that keeps writing past its own declared
			// Content-Length must be caught too: gzhttp removes the
			// Content-Length header once it starts compressing, so
			// net/http's own overflow enforcement never sees this on
			// the wire, and without this guard the extra bytes would
			// reach the client as a clean, longer compressed response.
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, strings.Repeat("y", 200))
		}, true},
		{"implicit 200, overlong", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, strings.Repeat("y", 200))
		}, true},
		{"short after 103", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set("Content-Length", "50")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, strings.Repeat("q", 25))
		}, true},
		{"full write", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "3")
			_, _ = io.WriteString(w, "abc")
		}, false},
		{"no Content-Length", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "abc")
		}, false},
		{"304 with Content-Length", http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "12345")
			w.WriteHeader(http.StatusNotModified)
		}, false},
		{"HEAD with Content-Length", http.MethodHead, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "999")
			w.WriteHeader(http.StatusOK)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := guardAborts(t, c.h, c.method); got != c.want {
				t.Fatalf("aborted = %v, want %v", got, c.want)
			}
		})
	}
}

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// server_test.go exercises newHTTPServer itself — the production wiring
// main() installs — rather than compress() or contentLengthGuard() in
// isolation (those have their own unit tests in gzip_test.go and clguard_test.go).
// Before newHTTPServer existed, nothing ran a request through the
// *http.Server main() actually builds: gzip_test.go's httptest servers
// called compress(...) directly, so a mutation to main's
// Handler: compress(r) wiring itself, or to any of its four timeout
// values, was invisible to the test suite. See PR #143 review.
//
// startTestServer wires an httptest.Server around the given *http.Server
// (as newHTTPServer returns it) rather than letting httptest build its
// own, so tests exercise the exact production Handler and timeouts.
func startTestServer(t *testing.T, srv *http.Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = srv
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// ginEngineWrapping adapts a plain http.Handler into a *gin.Engine so it
// can be passed to newHTTPServer, which (matching production's
// buildRouter signature) takes *gin.Engine. It registers the handler as
// a NoRoute fallback on an otherwise-empty engine, which is sufficient
// here: each test using it only ever requests the one path it registers
// on the wrapped mux.
func ginEngineWrapping(h http.Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	})
	return r
}

func noAutoDecompressHTTPClient(ts *httptest.Server) *http.Client {
	c := *ts.Client()
	tr := c.Transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	c.Transport = tr
	return &c
}

// Test_newHTTPServer_CompressesLargeResponses proves the real production
// Handler (compress(contentLengthGuard(r)), as newHTTPServer wires it)
// still gzip-compresses a large response end to end, not just that
// compress() does when called directly against a hand-built
// httptest.Server as gzip_test.go's tests do.
func Test_newHTTPServer_CompressesLargeResponses(t *testing.T) {
	big := []byte(strings.Repeat("a-fairly-long-value-for-compression-", 200))
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	})

	srv := newHTTPServer("0", ginEngineWrapping(mux))
	ts := startTestServer(t, srv)
	client := noAutoDecompressHTTPClient(ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/big", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("decompressed body mismatch: got %d bytes, want %d bytes", len(got), len(big))
	}
}

// Test_newHTTPServer_PinnedTimeouts pins the four slow-client timeout
// values documented on newHTTPServer's doc comment in main.go. A change
// to any one of them is a deliberate, reviewable decision, not an
// accidental refactor byproduct — this test is what would break if it
// happened silently.
func Test_newHTTPServer_PinnedTimeouts(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(func(c *gin.Context) { c.Next() }, fakeRouteCaller{}, fakeRouteMutator{}, logger)
	srv := newHTTPServer("8080", r)

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 10 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 60 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 900 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 120 * time.Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if want := ":8080"; srv.Addr != want {
		t.Errorf("Addr = %q, want %q", srv.Addr, want)
	}
}

// Test_newHTTPServer_TruncatedHandler_AbortsInsteadOfCleanEOF wires the
// exact production Handler (compress(contentLengthGuard(r))) and proves
// the two compose the way the truncation fix intends: a handler that
// announces Content-Length N but writes fewer bytes before returning
// must NOT reach the client as a clean, fully-decodable compressed
// response — the request must fail instead (a broken connection /
// unexpected EOF), both with and without Accept-Encoding.
func Test_newHTTPServer_TruncatedHandler_AbortsInsteadOfCleanEOF(t *testing.T) {
	full := []byte(strings.Repeat("x", 4096))
	half := full[:len(full)/2]

	mux := http.NewServeMux()
	mux.HandleFunc("/truncated", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(half)
	})

	for _, tc := range []struct {
		name           string
		acceptEncoding string
	}{
		{"with Accept-Encoding gzip", "gzip"},
		{"without Accept-Encoding", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHTTPServer("0", ginEngineWrapping(mux))
			ts := startTestServer(t, srv)
			client := noAutoDecompressHTTPClient(ts)

			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/truncated", nil)
			if tc.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tc.acceptEncoding)
			}
			resp, err := client.Do(req)
			if err != nil {
				// The connection was aborted before headers/trailer
				// completed — an acceptable failure shape too.
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatalf("expected a read error (truncated/aborted response), got a clean read")
			}
		})
	}
}

// Test_newHTTPServer_OverlongHandler_AbortsInsteadOfExtraBytes wires the
// exact production Handler (compress(contentLengthGuard(r))) and proves
// the other half of the truncation fix: a handler that announces a
// Content-Length and then writes MORE bytes than that before returning.
//
// Before PR #143's review comment 4143000404, contentLengthGuard only
// compared written < want, so this case slipped through silently: once
// gzhttp starts compressing, it removes the Content-Length header from
// what actually reaches the client, so net/http's own ErrContentLength
// overflow enforcement (which only fires when that header is really on
// the wire) never sees a mismatch, and the extra bytes would be framed
// straight into a clean, fully-decodable, longer compressed response.
func Test_newHTTPServer_OverlongHandler_AbortsInsteadOfExtraBytes(t *testing.T) {
	const declared = 4096
	overlong := []byte(strings.Repeat("x", declared+100))

	newOverlongHandler := func() http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(declared))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(overlong)
		}
	}

	t.Run("with Accept-Encoding gzip", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/overlong", newOverlongHandler())
		srv := newHTTPServer("0", ginEngineWrapping(mux))
		ts := startTestServer(t, srv)
		client := noAutoDecompressHTTPClient(ts)

		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/overlong", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := client.Do(req)
		if err != nil {
			// The connection was aborted before headers/trailer
			// completed — an acceptable failure shape too.
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if body, err := io.ReadAll(resp.Body); err == nil {
			t.Fatalf("expected a read/decode error for an overlong compressed response, got a clean %d-byte read", len(body))
		}
	})

	// Without Accept-Encoding, gzhttp never starts compressing, so
	// nothing about this guard should change what a plain net/http
	// server running the exact same handler (no guard, no gzhttp) would
	// do with an overlong write — that is net/http's own behaviour to
	// own, not this guard's job to alter. Compare outcomes against a
	// bare net/http server rather than asserting one specific shape, per
	// the review comment's "don't assert more than you verify".
	t.Run("without Accept-Encoding", func(t *testing.T) {
		plainTS := httptest.NewServer(newOverlongHandler())
		t.Cleanup(plainTS.Close)
		plainResp, plainErr := plainTS.Client().Get(plainTS.URL)
		var plainBody []byte
		var plainReadErr error
		if plainErr == nil {
			defer func() { _ = plainResp.Body.Close() }()
			plainBody, plainReadErr = io.ReadAll(plainResp.Body)
		}

		mux := http.NewServeMux()
		mux.HandleFunc("/overlong", newOverlongHandler())
		srv := newHTTPServer("0", ginEngineWrapping(mux))
		ts := startTestServer(t, srv)
		resp, err := noAutoDecompressHTTPClient(ts).Get(ts.URL + "/overlong")
		var body []byte
		var readErr error
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			body, readErr = io.ReadAll(resp.Body)
		}

		plainFailed := plainErr != nil || plainReadErr != nil
		gotFailed := err != nil || readErr != nil
		if plainFailed != gotFailed {
			t.Fatalf("outcome differs from plain net/http: plain failed=%v (err=%v, readErr=%v), production wiring failed=%v (err=%v, readErr=%v)",
				plainFailed, plainErr, plainReadErr, gotFailed, err, readErr)
		}
		if !plainFailed && !bytes.Equal(plainBody, body) {
			t.Fatalf("body differs from plain net/http: plain = %d bytes, production wiring = %d bytes", len(plainBody), len(body))
		}
	})
}

// Test_newHTTPServer_GinStreamAndHijack_Work pins the two gin features
// that reach the writer below gin through UNCHECKED type assertions:
// c.Stream (CloseNotify) and c.Writer.Hijack (websocket upgrades). Both
// must work through the production wiring with and without
// Accept-Encoding (gzhttp picks a different writer for each).
func Test_newHTTPServer_GinStreamAndHijack_Work(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/stream", func(c *gin.Context) {
		n := 0
		c.Stream(func(w io.Writer) bool {
			_, _ = io.WriteString(w, "data: x\n\n")
			n++
			return n < 3
		})
	})
	r.GET("/hijack", func(c *gin.Context) {
		conn, bw, err := c.Writer.Hijack()
		if err != nil {
			c.String(http.StatusInternalServerError, "hijack: %v", err)
			return
		}
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = bw.Flush()
		_ = conn.Close()
	})
	ts := startTestServer(t, newHTTPServer("0", r))

	for _, ae := range []string{"", "gzip"} {
		for path, want := range map[string]string{
			"/stream": "data: x\n\ndata: x\n\ndata: x\n\n",
			"/hijack": "hijacked",
		} {
			t.Run(path+" Accept-Encoding="+ae, func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
				if ae != "" {
					req.Header.Set("Accept-Encoding", ae)
				}
				// ts.Client() decodes gzip transparently only when it set
				// Accept-Encoding itself, so decode by hand when needed.
				resp, err := noAutoDecompressHTTPClient(ts).Do(req)
				if err != nil {
					t.Fatalf("do request: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				var body io.Reader = resp.Body
				if resp.Header.Get("Content-Encoding") == "gzip" {
					zr, err := gzip.NewReader(resp.Body)
					if err != nil {
						t.Fatalf("gzip.NewReader: %v", err)
					}
					body = zr
				}
				got, err := io.ReadAll(body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if resp.StatusCode != http.StatusOK || string(got) != want {
					t.Fatalf("got %d %q, want 200 %q", resp.StatusCode, got, want)
				}
			})
		}
	}
}

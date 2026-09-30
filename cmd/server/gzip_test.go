package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// gzip_test.go exercises the exact compress() wrapper cmd/server/main.go
// installs as http.Server.Handler — the one and only place gzip compression
// is applied (see compress's doc comment in main.go for why it wraps the
// http.Handler rather than being a Gin middleware, and why gzhttp's zstd
// default is kept rather than restricted to gzip-only).
//
// Two servers back these tests:
//
//   - appServer wraps the REAL buildRouter (same fakes as router_test.go)
//     with compress(), so /healthz, /api/me and unknown-route 404 behaviour
//     is exercised exactly as main.go wires it.
//   - muxServer wraps a tiny purpose-built http.ServeMux with the SAME
//     compress() call. buildRouter's own routes have fixed, small response
//     bodies, so this is the only way to exercise size- and
//     already-encoded-response behaviour deterministically without
//     changing (and thereby breaking Test_RouterAllowList's pinned route
//     set) buildRouter itself.

// bigJSONBody is comfortably above gzhttp's 1024-byte DefaultMinSize.
var bigJSONBody = mustJSON(map[string]any{
	"claims": strings.Repeat("a-fairly-long-claim-value-", 100),
})

// hugeJSONBody is a ~5 MB time-series-like JSON payload: repetitive enough
// that gzip should shrink it a lot, but shaped like real telemetry rather
// than one repeated byte.
var hugeJSONBody = mustHugeTimeSeriesJSON(60000)

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mustHugeTimeSeriesJSON(points int) []byte {
	type point struct {
		Timestamp int64   `json:"timestamp"`
		Value     float64 `json:"value"`
		Sensor    string  `json:"sensor"`
	}
	series := make([]point, points)
	for i := range series {
		series[i] = point{
			Timestamp: 1_700_000_000 + int64(i),
			Value:     float64(i%1000) * 0.125,
			Sensor:    fmt.Sprintf("sensor-%d", i%7),
		}
	}
	return mustJSON(series)
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// alreadyEncodedUpstreamBody simulates a handler that already relayed an
// upstream gzip-compressed body byte-for-byte (e.g. ginpingo.ProxyHandler
// forwarding a gzip-encoded SAP response, or any handler that sets
// Content-Encoding itself). compress() must pass such a response through
// unmodified rather than double-compressing it.
var alreadyEncodedPlaintext = []byte(`{"already":"gzip-encoded-by-the-handler-itself"}`)

func newMuxServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bigJSONBody)
	})

	mux.HandleFunc("/huge", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(hugeJSONBody)
	})

	mux.HandleFunc("/tiny", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/already-encoded", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBytes(t, alreadyEncodedPlaintext))
	})

	srv := httptest.NewServer(compress(mux))
	t.Cleanup(srv.Close)
	return srv
}

func newAppServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := buildRouter(func(c *gin.Context) { c.Next() }, fakeRouteCaller{}, fakeRouteMutator{}, logger)
	srv := httptest.NewServer(compress(r))
	t.Cleanup(srv.Close)
	return srv
}

// readBodyDecodingGzipIfNeeded reads the raw bytes off the wire (Go's
// http.Transport does its OWN transparent gzip decoding when the request
// did not set Accept-Encoding itself, which would hide the exact behaviour
// under test) by disabling that via a Transport with DisableCompression,
// then manually gunzips when Content-Encoding says gzip.
func readRaw(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw
}

func gunzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}

// noAutoDecompressClient returns an *http.Client whose Transport does NOT
// transparently request/decode gzip on our behalf (the stdlib does this by
// default whenever the caller hasn't set Accept-Encoding, which would make
// every assertion below about Content-Encoding meaningless).
func noAutoDecompressClient(srv *httptest.Server) *http.Client {
	c := *srv.Client()
	tr := c.Transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	c.Transport = tr
	return &c
}

func Test_Compress_JSONAboveMinSize_Gzip(t *testing.T) {
	srv := newMuxServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/big", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want it to include Accept-Encoding", resp.Header.Get("Vary"))
	}

	raw := readRaw(t, resp)
	got := gunzip(t, raw)
	if !bytes.Equal(got, bigJSONBody) {
		t.Fatalf("decompressed body mismatch: got %d bytes, want %d bytes", len(got), len(bigJSONBody))
	}
}

func Test_Compress_NoAcceptEncoding_Uncompressed(t *testing.T) {
	srv := newMuxServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/big", nil)
	// deliberately no Accept-Encoding header
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty (uncompressed)", got)
	}
	raw := readRaw(t, resp)
	if !bytes.Equal(raw, bigJSONBody) {
		t.Fatalf("body mismatch: got %d bytes, want %d bytes identical to source", len(raw), len(bigJSONBody))
	}
}

func Test_Compress_GzipQZero_Uncompressed(t *testing.T) {
	srv := newMuxServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/big", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty (client explicitly rejected gzip)", got)
	}
	raw := readRaw(t, resp)
	if !bytes.Equal(raw, bigJSONBody) {
		t.Fatalf("body mismatch: got %d bytes, want %d bytes identical to source", len(raw), len(bigJSONBody))
	}
}

// Test_Compress_UnknownRoute_404Body is the regression test for the bug
// that killed the earlier gin-level ginpingo.Gzip() approach: a gzipped
// response for a route Gin doesn't recognise must still carry gin's real
// 404 body, not an empty one.
func Test_Compress_UnknownRoute_404Body(t *testing.T) {
	srv := newAppServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/this-route-does-not-exist", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	raw := readRaw(t, resp)
	var body []byte
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body = gunzip(t, raw)
	} else {
		body = raw
	}
	if len(body) == 0 {
		t.Fatalf("404 body is empty; want gin's real not-found body")
	}
}

// Test_Compress_AlreadyEncodedResponse_PassedThrough covers a handler that
// already set Content-Encoding itself before compress() ever sees the
// response — e.g. ginpingo.ProxyHandler relaying an upstream SAP response
// that was already gzip-encoded. compress() must pass it through
// byte-identical, never double-compress it.
func Test_Compress_AlreadyEncodedResponse_PassedThrough(t *testing.T) {
	srv := newMuxServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/already-encoded", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (the handler's own, not a wrapper-added one)", got)
	}

	raw := readRaw(t, resp)
	want := gzipBytes(t, alreadyEncodedPlaintext)
	// Not asserting byte-identical gzip framing (gzip.Writer options could
	// differ), but the decompressed payload MUST be exactly what the
	// handler wrote once, not compressed a second time.
	got := gunzip(t, raw)
	wantPlain := gunzip(t, want)
	if !bytes.Equal(got, wantPlain) {
		t.Fatalf("decompressed body mismatch: got %q want %q", got, wantPlain)
	}
	// A double-compressed body gunzips to bytes that are themselves a
	// valid-looking gzip stream's worth of noise, not our JSON — assert
	// directly that a single gunzip already yields the original plaintext.
	if !bytes.Equal(got, alreadyEncodedPlaintext) {
		t.Fatalf("body was double-compressed or corrupted: got %q", got)
	}
}

func Test_Compress_LargeBody_RoundTripsAndShrinks(t *testing.T) {
	srv := newMuxServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/huge", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}

	raw := readRaw(t, resp)
	if len(raw) >= len(hugeJSONBody) {
		t.Fatalf("compressed size %d not smaller than uncompressed size %d", len(raw), len(hugeJSONBody))
	}
	// Repetitive time-series JSON should compress very well; guard against
	// a regression that silently stops actually compressing (e.g. minSize
	// misconfigured so the handler falls back to pass-through) by requiring
	// a substantial reduction, not just "any smaller".
	if ratio := float64(len(raw)) / float64(len(hugeJSONBody)); ratio > 0.5 {
		t.Fatalf("compressed size %d is only %.2fx smaller than uncompressed %d; expected well below 0.5x for repetitive JSON",
			len(raw), ratio, len(hugeJSONBody))
	}

	got := gunzip(t, raw)
	if !bytes.Equal(got, hugeJSONBody) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d bytes", len(got), len(hugeJSONBody))
	}
}

func Test_Compress_SmallBody_NotCompressed(t *testing.T) {
	// /healthz on the REAL router: "ok", well under gzhttp's DefaultMinSize.
	srv := newAppServer(t)
	client := noAutoDecompressClient(srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for a tiny body", got)
	}
	raw := readRaw(t, resp)
	if string(raw) != "ok" {
		t.Fatalf("body = %q, want %q", raw, "ok")
	}

	// Belt-and-braces: the purpose-built /tiny route on the mux server too.
	muxSrv := newMuxServer(t)
	muxClient := noAutoDecompressClient(muxSrv)
	req2, _ := http.NewRequest(http.MethodGet, muxSrv.URL+"/tiny", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	resp2, err := muxClient.Do(req2)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if got := resp2.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for a tiny body", got)
	}
}

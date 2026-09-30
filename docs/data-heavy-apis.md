# Data-heavy APIs

This is for a fork whose endpoints return a lot of data — the shape that motivated it was a time
series exported from an on-premise ABAP backend, one HTTP response per requested window. It
assembles what's already scattered across the README and `btpingo`'s doc comments into one place,
plus new advice on paging, streaming and payload shape. It does not replace those sections — where
something is already documented (timeouts, `GOMEMLIMIT`, compression), this links to it instead of
restating it.

The ABAP-side half of this is [`docs/abap-backend-playbook.md`'s "Data-heavy endpoints"
section](abap-backend-playbook.md#4-data-heavy-endpoints-considerations). Read both if you're
designing the endpoint end to end; they're written for different audiences (this one for the Go
side, that one for whoever writes the ABAP side) and deliberately don't repeat each other.

## The on-premise response cap

`btpingo.DefaultOnPremResponseSizeLimit` caps how much of an on-premise response `*btpingo.Service`
lets a caller read: 10 MiB (`10 << 20` bytes), defined in `btpingo`'s `service.go`. It exists so a
misrouted hostname, a misbehaving SAP backend, or — for a customer-managed Cloud Connector — a MITM
between the connector and SAP can't stream unbounded data into the app's memory quota. A read past
the cap returns `btpingo.ErrOnPremResponseTooLarge`.

**A data-heavy endpoint needs to raise this per service instance**, not disable it. Pass
`btpingo.WithOnPremResponseSizeLimit(bytes)` to `btpingo.NewService(env, ...)` when constructing the
`*btpingo.Service` for that route — a zero or negative value resets to the 10 MiB default, it
doesn't turn the cap off. Give the dedicated instance a limit sized to your largest expected page,
with headroom, not to "however big responses can theoretically get" — pair the limit with paging (see
below) rather than raising it to cover an unbounded response.

**Streaming does not bypass the cap.** The size limit is enforced by a wrapper around
`resp.Body` (`newLimitedOnPremBody` in `service.go`) that both `CallOnPremise` and
`CallOnPremiseMutating` apply unconditionally, before the caller ever sees the body — it doesn't
matter whether the caller buffers with `io.ReadAll` or streams with `io.Copy` /
`c.DataFromReader`. The consequence differs by which one you use, though:

- **Buffering** (`io.ReadAll`): the error surfaces from the `ReadAll` call itself, before any
  response has gone to the client. A handler can still turn it into a clean `502`.
- **Streaming** (`c.DataFromReader`, `io.Copy`, `ginpingo.ProxyHandler`): the `200` and headers are
  already sent when the cap trips. If a `Content-Length` was forwarded (SAP sent one and Go's
  transport didn't decompress the body), `contentLengthGuard` in `cmd/server/main.go` sees the short
  write and aborts the connection, so the client gets an unexpected EOF, with or without
  compression. If there is none (SAP answered chunked, or gzipped so that Go's transport
  decompressed it and `resp.ContentLength` is `-1`, which is what a typed handler gets whenever SAP
  compresses), the response ends cleanly: a proper final chunk or a complete gzip/zstd stream, just
  shorter. No HTTP-level check
  can tell it apart from a complete response. Only the payload itself can (see "Payload shape").
  The same happens if `DefaultOnPremiseTimeout` fires mid-body (see "Timeouts").

**What the cap counts depends on who asked for compression.** A typed handler (all three examples)
forwards no `Accept-Encoding`, so Go's transport requests gzip itself, decompresses transparently,
and the cap measures **decompressed** bytes. `ginpingo.ProxyHandler` forwards the client's
`Accept-Encoding`. If SAP compresses, the body is passed through undecoded and the cap measures
**compressed** bytes, so the same endpoint can succeed for a browser and hit the cap for `curl`
without `--compressed`. Size the limit for the decompressed payload.

## Buffering vs. streaming

This repo's three example handlers show both patterns:

- [examples/adtdiscovery](../examples/adtdiscovery/handler.go) and
  [examples/adtcheckrun](../examples/adtcheckrun/handler.go) call `io.ReadAll(resp.Body)`, then
  unmarshal the buffered bytes (XML in both cases) and re-marshal a reshaped JSON view. Fine for the
  small, fixed-shape ADT payloads these handlers target.
- [examples/invoicesync](../examples/invoicesync/handler.go) calls
  `c.DataFromReader(resp.StatusCode, resp.ContentLength,
resp.Header.Get("Content-Type"), resp.Body, nil)` — it streams SAP's response bytes straight
  through without buffering or reshaping them.

The difference is a size-driven choice, and each pattern has a real limit:

- **`io.ReadAll` + reshape** only works while the buffered size stays comfortably inside the
  process's memory budget. Reshaping (XML → JSON, or any other transform) that needs the whole
  document in memory at once — as the ADT examples' XML unmarshalling does — doesn't have a small
  code change that fixes this; a genuinely large payload needs a streaming decoder/encoder pair
  instead, which is a different, more involved implementation than either example ships.
- **`c.DataFromReader` verbatim streaming** (`invoicesync`'s pattern) only works when the on-premise
  system already emits exactly the bytes the client should receive — it can't reshape the payload
  in flight. Note `invoicesync` is a reference example only: it is not wired into `cmd/server`'s
  router today (see the comment at the top of `examples/adtdiscovery/handler.go`, which explains why
  only `adtdiscovery`/`adtcheckrun` are).

**Memory math.** `manifest.yml`'s backend app defaults to `memory: 128M` with
`GOMEMLIMIT: 100MiB` — see [the "Body-size cap" section](../README.md#body-size-cap) for why the two
are tied together and must be raised as a pair. Budget about **3× the payload per in-flight buffered
request**, not 1×: the raw bytes, the decoded structs, and for a reshaping handler the re-encoded
output. Measured with go1.27: a 20 MB JSON time-series array reached 47–64 MiB of peak heap for
`ReadAll` + `Unmarshal` (+ `Marshal`). That is more than half of the default `GOMEMLIMIT` for one
request, so at the defaults a second concurrent one pushes the process into GC thrashing or an OOM
kill. Size `memory:`/`GOMEMLIMIT` for `(max concurrent requests on the route) × 3 × (raised limit)`.
Streaming keeps a single request's incremental memory use close to `io.Copy`'s buffer size (32 KiB
per copy by default, plus the per-response compressor state; gzhttp's zstd encoder uses a 128 KiB
window) regardless of the total response size, which is what makes it the correct choice once
responses stop being small and fixed-size.

## Timeouts across the chain

The Go-side pair — `http.Server.WriteTimeout` (900 s) and `btpingo.DefaultOnPremiseTimeout` (10
min, asymmetric on purpose) — is already documented in [the "Timeouts — three layers, two of them
ours" section](../README.md#timeouts--three-layers-two-of-them-ours); this doesn't repeat that
table. What that section adds for the latency case applies here for the payload-size case too: a
large response that's slow to write can exhaust `WriteTimeout` even if the on-premise call itself
returned promptly, and a single route that legitimately needs longer gets the same escape hatch —
`http.NewResponseController(w).SetWriteDeadline(...)` on the server side, paired with a dedicated
`*btpingo.Service` built via `btpingo.WithOnPremiseTimeout(...)` for that route, rather than
loosening the global defaults.

For a streamed response the whole body transfer counts against `DefaultOnPremiseTimeout` too, since
`http.Client.Timeout` includes reading the body: a stream still running 600 s after the request
started is cut mid-body, before the 900 s `WriteTimeout` is reached, and ends the same way as a cap
hit (see ["The on-premise response cap"](#the-on-premise-response-cap)).

Two more hops sit in the path besides the CF Gorouter (covered in that README section). **The
approuter** (`@sap/approuter` 23.0.0) gives each destination a default `timeout` of 30 000 ms, an
inactivity timer on the backend connection that answers 504 when it fires, and that cuts the body
short if the stall happens after the `200` has gone out. `manifest.yml`'s `GoBackend` destination sets none, so a request through the approuter
fails after 30 s of silence from this backend, long before `DefaultOnPremiseTimeout` or
`WriteTimeout` matter. Nothing flows until SAP has answered, and an ABAP handler answers only once
the whole page is built. For a slow route, add `"timeout": <ms>` to the `GoBackend` entry in
`manifest.yml`, or keep pages small enough to answer well inside 30 s. **The Cloud Connector** may
have its own ceiling. This repo doesn't set it — check for your landscape.

## Compression

[The "Response compression" section](../README.md#response-compression) covers what `gzhttp` does
at the `http.Server.Handler` level — this doesn't restate it. One thing specific to a proxied
on-premise response:

- `ginpingo.ProxyHandler` forwards the inbound request's `Accept-Encoding` header to the on-premise
  call (it's not in the internal header-drop list `internal/fwdheader` applies to both the outbound
  request and the relayed response), and relays the on-premise response's `Content-Encoding` header
  back to the client unchanged. If ABAP already gzips its response, that compression survives the
  proxy hop as-is.

## Payload shape for time series

- **NDJSON (or CSV) with an explicit trailer.** Rows can be produced and consumed one at a time. On
  the Go side a JSON array can be streamed too (`json.Decoder.Token`/`More`), but NDJSON also lets a
  client use rows before the response ends. Neither format makes truncation reliably detectable: a
  cut on a line boundary looks complete. So end every page with a trailer record, e.g.
  `{"end":true,"rows":N,"next":"<cursor>"}`, and have clients treat a missing trailer as failure. For
  a verbatim stream (`invoicesync`'s pattern) the trailer has to come from the ABAP side.
- **Timestamps:** pick one format and use it consistently end to end — ISO 8601 in UTC (e.g.
  `2026-09-30T12:00:00Z`) is the common choice and avoids ambiguity around local time / DST that a
  Go client, a JS client and an ABAP-side date/time pair can each get wrong differently. This repo
  doesn't currently standardize a timestamp format anywhere else, so this is a decision your fork
  makes once and documents for its own API.
- **Decimals as JSON strings.** Most parsers (JavaScript's, Go's default decoding into `float64`)
  read JSON numbers as IEEE-754 doubles; a SAP `DEC`/`CURR`/`QUAN` value
  round-tripped through a JSON number can lose precision on the low digits. Send decimals as
  strings (`"123.45"` rather than `123.45`) if exact round-tripping matters for the field, and say so
  in your API contract so clients parse them as decimals rather than floats.

## Paging

Recommend a paging contract over one huge response: page by time window (e.g. `from`/`to` query
parameters bounding a page) or by a keyset cursor (an opaque cursor carrying the last row's key
forward to the next request). The corresponding ABAP-side considerations — what
bounds a page's size and processing time on that side — are in [the playbook's "Data-heavy
endpoints" section](abap-backend-playbook.md#4-data-heavy-endpoints-considerations); the format
(timestamps, decimals) has to agree between the two sides, which is exactly the contract described
above.

Make the cursor a **unique** key: if several series share a timestamp, carry `(series id,
timestamp)`, not the timestamp alone, or rows are lost or duplicated at page boundaries. Use
half-open windows (`from` inclusive, `to` exclusive). Enforce a server-side maximum page size that
fits under the size cap and the approuter timeout.

## Sizing memory and instances

Raise `memory:` and `GOMEMLIMIT` together, as already covered in [the "Body-size cap"
section](../README.md#body-size-cap) — this doesn't restate the gate that enforces it. Size the
increase for the memory math above: the worst case is concurrent large in-flight requests, not one
request at a time, so a route that's expected to see concurrent traffic needs proportionally more
headroom than a single-worst-case response size would suggest. How much memory and how many
instances a given CF org/space plan or quota actually allows — **check for your landscape**.

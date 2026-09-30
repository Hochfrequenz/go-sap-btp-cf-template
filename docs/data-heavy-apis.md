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

Everything below was checked against `github.com/hochfrequenz/btpingo v0.1.1` (the version pinned
in this repo's `go.mod`) and this repo at commit `03297a9`.

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
- **Streaming** (`io.Copy`, `c.DataFromReader`, or `ginpingo.ProxyHandler`, which uses `io.Copy`
  internally): the `200` status and headers are already written by the time the copy trips the
  cap, so the client gets a **truncated body under a success status**. With a `Content-Length`
  header the client at least sees a short read and can detect it; a chunked response (no
  `Content-Length`, which is what `c.DataFromReader` sends when `resp.ContentLength` is `-1`) can
  end up looking like a complete response to a client that doesn't check for a terminating
  marker. Build your response format so truncation is detectable (see "Payload shape" below), or
  keep pages small enough that hitting the cap mid-page is not an expected occurrence you need to
  detect.

The response write itself also counts against the server's `WriteTimeout` — a large page that's
slow to write can hit that timeout as well as, or instead of, the size cap.

## Buffering vs. streaming

This repo's three example handlers show both patterns:

- `examples/adtdiscovery` and `examples/adtcheckrun` call `io.ReadAll(resp.Body)`, then unmarshal
  the buffered bytes (XML in both cases) and re-marshal a reshaped JSON view. Fine for the small,
  fixed-shape ADT payloads these handlers target.
- `examples/invoicesync` calls `c.DataFromReader(resp.StatusCode, resp.ContentLength,
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
  in flight. It's also the right template to copy for `ginpingo.ProxyHandler`-style transparent
  proxying. Note `invoicesync` is a reference example only: it is not wired into `cmd/server`'s
  router today (see the comment at the top of `examples/adtdiscovery/handler.go`, which explains why
  only `adtdiscovery`/`adtcheckrun` are).

**Memory math.** `manifest.yml`'s backend app defaults to `memory: 128M` with
`GOMEMLIMIT: 100MiB` — see [the "Body-size cap" section](../README.md#body-size-cap) for why the two
are tied together and must be raised as a pair. Buffering holds one full response in memory per
in-flight request; with concurrent requests, that's roughly `(number of concurrent large requests) ×
(response size)` competing for the same `GOMEMLIMIT` budget, on top of everything else the process
holds (goroutine stacks, the JSON encoder's own buffers, HTTP/response framing, GC headroom).
Streaming keeps a single request's incremental memory use close to `io.Copy`'s buffer size (32 KiB
per copy by default) regardless of the total response size, which is what makes it the correct
choice once responses stop being small and fixed-size. If you do raise `WithOnPremResponseSizeLimit`
and buffer anyway, size `memory:`/`GOMEMLIMIT` for the worst case of `(max concurrent requests to
that route) × (raised limit)`, not for one request at a time.

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

Two more hops sit in the same request path and are not something this repo's code sets or measures:
the **Cloud Connector** tunnel and the **approuter**. The README's Timeouts section already notes
that CF's Gorouter has its own per-request ceiling (typically around 900 s, varies by foundation)
that the 900 s `WriteTimeout` is aligned with. Whether the Cloud Connector or the approuter impose a
tighter ceiling of their own, and what a specific BTP subaccount's Gorouter timeout actually is —
**check for your landscape**; this repo doesn't pin a verified number for either.

## Compression

[The "Response compression" section](../README.md#response-compression) covers what `gzhttp` does
at the `http.Server.Handler` level (zstd/gzip negotiated per `Accept-Encoding`, `contentLengthGuard`
closing the length-mismatch gap). Two things specific to a proxied on-premise response:

- `ginpingo.ProxyHandler` forwards the inbound request's `Accept-Encoding` header to the on-premise
  call (it's not in the internal header-drop list `internal/fwdheader` applies to both the outbound
  request and the relayed response), and relays the on-premise response's `Content-Encoding` header
  back to the client unchanged. If ABAP already gzips its response, that compression survives the
  proxy hop as-is.
- `gzhttp` already skips a response a handler already compressed itself — a proxied on-premise body
  that already carries a `Content-Encoding` isn't compressed a second time.

For a data-heavy endpoint that doesn't use `ginpingo.ProxyHandler` (a typed handler that reshapes
the response, for instance), the gzip/zstd wrapping at the `http.Server.Handler` level applies the
same way regardless of how the handler builds its response body.

## Payload shape for time series

- **NDJSON or CSV over a single large JSON array.** A JSON array needs the whole array serialized
  (or a streaming JSON array encoder, which most standard-library-adjacent JSON code doesn't give
  you for free) before the closing `]` — the writer effectively needs the whole page assembled. NDJSON
  (one JSON object per line) and CSV can both be written and read one row at a time, which matches a
  keyset-paginated or streamed source row-for-row and also gives truncation a detectable shape: a
  response cut off mid-stream ends with a partial line instead of ending exactly where the array's
  closing bracket should be.
- **Timestamps:** pick one format and use it consistently end to end — ISO 8601 in UTC (e.g.
  `2026-09-30T12:00:00Z`) is the common choice and avoids ambiguity around local time / DST that a
  Go client, a JS client and an ABAP-side date/time pair can each get wrong differently. This repo
  doesn't currently standardize a timestamp format anywhere else, so this is a decision your fork
  makes once and documents for its own API.
- **Decimals as JSON strings.** JSON numbers are IEEE-754 doubles; a SAP `DEC`/`CURR`/`QUAN` value
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

## Sizing memory and instances

Raise `memory:` and `GOMEMLIMIT` together, as already covered in [the "Body-size cap"
section](../README.md#body-size-cap) — do not raise one without the other, and the
`GOMEMLIMIT`-vs-`memory:` gate in `.github/workflows/template-guards.yml` will fail the build if
`GOMEMLIMIT` is missing, malformed, `off`, below 16 MiB, or not strictly below `memory:`. Size the
increase for the memory math above: the worst case is concurrent large in-flight requests, not one
request at a time, so a route that's expected to see concurrent traffic needs proportionally more
headroom than a single-worst-case response size would suggest. How much memory and how many
instances a given CF org/space plan or quota actually allows — **check for your landscape**.

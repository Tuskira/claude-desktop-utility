# Claude Desktop Utility

A small TLS-intercepting (MITM) proxy for macOS that observes Claude Desktop's traffic and
forwards its Code tab and Chat tab activity to a Tuskira gateway as LLM call and access log
records. It signs per-host certificates with a locally generated CA. It only **views and logs**
traffic; it never modifies it. Standard library only, no dependencies.

Everything else Claude Desktop does (presence, heartbeats, telemetry, ...), and any other app's
traffic if you point it at the interceptor, is proxied exactly as normal but never forwarded
anywhere. Gateway forwarding is not optional: `interceptor run` refuses to start unless it is
configured (see [Flags](#flags) below), so a misconfigured install can never silently run without
anywhere to send what it observes.

## Quick start

The one supported way to run this against Claude Desktop — building, trusting the local CA,
creating a gateway key, installing the background service, and pointing Claude Desktop at it — is
[deploy/macos/README.md](deploy/macos/README.md). Start there.

To run the CLI directly instead (for scripting, or to point it at some other app):

```sh
make build                            # -> ./bin/interceptor
./bin/interceptor ca init             # writes ~/.interceptor/ca.pem and ca-key.pem
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.interceptor/ca.pem
echo -n "gk_..." > ~/.interceptor/gateway.key && chmod 600 ~/.interceptor/gateway.key
./bin/interceptor run --gateway-url https://your-gateway.example.com --gateway-key-file ~/.interceptor/gateway.key
```

`make check` runs vet, tests, and a gofmt check. `ca init` refuses to overwrite an existing CA
unless you pass `--force`; `ca path` prints the certificate path; `interceptor run` creates the CA
automatically if it is missing. Keep `ca-key.pem` private (mode 0600) — anyone holding it can
intercept traffic from a machine that trusts the CA. Remove the trust when finished:
`sudo security delete-certificate -c "Interceptor Local CA" /Library/Keychains/System.keychain`.

Firefox, and many Node/Electron, Python and other apps, do not use the macOS keychain. Point them
at the certificate instead: `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` all
work, set to `~/.interceptor/ca.pem`. Claude Desktop does not need this; it uses the system trust
store, covered in Part 1 of the install guide above.

## Flags

```sh
./bin/interceptor run --gateway-url https://your-gateway.example.com --gateway-key-file ~/.interceptor/gateway.key
./bin/interceptor run -v --filter-host '*.anthropic.com' --gateway-url ... --gateway-key-file ...
```

| Flag | Default | Meaning |
|---|---|---|
| `--gateway-url` | **required** | gateway control-plane base URL, e.g. `https://your-gateway.example.com`. `interceptor run` refuses to start without it |
| `--gateway-key-file` | **required**\* | path to a file holding the gateway key, mode `0600` (refuses to start otherwise). \*Alternative: the `INTERCEPTOR_GATEWAY_KEY` environment variable |
| `--dir` | `~/.interceptor` | CA directory (auto `ca init` if missing) |
| `--proxy-addr` | `127.0.0.1:8080` | explicit HTTP proxy (plain forward and `CONNECT`); empty disables |
| `--out` | `capture.jsonl` | JSONL file (append mode); `--out ""` disables |
| `--max-body` | `1MB` | captured bytes per message (`512KB`, `2MB`, plain bytes). Bodies are still fully proxied; only the logged copy is cut and flagged `*_truncated` |
| `--filter-host` | none | repeatable glob; only matching hosts are logged. Everything is still proxied |
| `--passthrough` | none | repeatable glob; tunnel these hosts raw with no interception. Matches `host` or `host:port` |
| `--no-stream-records` | off | do not write per-event/envelope/line records for streaming responses |
| `-v` | off | print headers and bodies too |
| `--insecure-upstream` | off | skip upstream certificate verification (dev only) |
| `--spool-dir` | `~/.interceptor/spool` | directory for records that could not be forwarded yet (gateway down, network error); replayed automatically, capped at 500 MB / 7 days |
| `--forward-user` | empty | user label sent with forwarded records, overriding the Claude account email the interceptor otherwise learns from traffic |

Advanced, and not part of the supported Claude Desktop setup (the code exists for other uses of
the proxy; see [deploy/macos/README.md](deploy/macos/README.md) for how Claude Desktop is actually
routed, via its own `egressProxyUrl` setting):

| Flag | Default | Meaning |
|---|---|---|
| `--transparent-addr` | empty (off) | raw TLS listener for `pf`-redirected traffic, e.g. `127.0.0.1:8443`. Upstream host comes from SNI, port 443 |
| `--transparent-http-addr` | empty (off) | raw plain-HTTP listener for redirected traffic. Upstream host comes from `Host`, port 80 |

The client side only negotiates HTTP/1.1, so clients never speak HTTP/2 to the interceptor. Upstream
may still use HTTP/2.

## What's captured, forwarded, and redacted

The interceptor turns Claude Desktop's Code tab and Chat tab traffic into gateway-shaped LLM call
and access log records and pushes them to the gateway's `POST /api/v1/ingest`. Every other app's
traffic, and non-Code/Chat Claude Desktop traffic (presence, heartbeats, telemetry, ...), is
proxied exactly as before but never forwarded. See `docs/design/interceptor-ingest.md` in the
`ai-agent-gateway` repo for the full design.

- The Claude account email is read automatically from a captured sign-in response; `--forward-user`
  overrides it.
- Records are redacted (secrets masked, device-attestation payloads stripped) before they are queued,
  batched (up to 500 records or every 2 seconds), gzipped, and POSTed. If the gateway is unreachable or
  busy, batches are spooled to disk (`--spool-dir`, default `~/.interceptor/spool`) and retried with
  backoff; nothing is lost across a restart, and nothing is double-sent (the gateway de-duplicates by
  request ID). A status line every 60 seconds on stderr reports sent / duplicate / rejected / dropped /
  spooled counts.
- Connector (MCP) tool calls are forwarded as access log records; built-in tools (Bash, Edit, ...) are
  not. Chat tab token counts are always 0 (not available outside the Code tab). Cost is always left for
  the gateway to compute; the interceptor never sends one.
- `capture.jsonl` (see `--out`) is the full local record, independent of forwarding: one JSON object
  per exchange, used for debugging and for `interceptor decode`. See
  [Reference: reading capture.jsonl](#reference-reading-capturejsonl) below for its format.

## Security

- **The CA private key** (`~/.interceptor/ca-key.pem`) lets anyone holding it intercept TLS traffic
  from any machine that trusts the corresponding CA certificate. Keep it mode 0600 and never commit
  or share it. Remove the CA from system trust when you are done:
  `sudo security delete-certificate -c "Interceptor Local CA" /Library/Keychains/System.keychain`.
- **The local proxy has no authentication.** Anything on the machine that can reach `127.0.0.1:9090`
  (or whichever `--proxy-addr` you chose) can send traffic through it. This is fine on a single-user
  laptop; do not expose the listen address beyond localhost.
- **The capture file is a password file.** `capture.jsonl` (default `~/claude-capture.jsonl` in the
  supported install) holds full chats and session tokens in the clear; it is created mode 0600.
  Treat it accordingly, and delete or rotate it periodically.
- **The gateway key** (`~/.interceptor/gateway.key`) authenticates the interceptor to the gateway;
  keep it mode 0600 (`run` refuses to start otherwise) and scoped to the `interceptor` role.
- **Use an `https://` gateway URL in production.** `--gateway-url http://...` is accepted for local
  development against a gateway on localhost, but the gateway key travels in a request header on
  every batch, so an `http://` URL to anything other than localhost sends it in the clear.

## Troubleshooting

- **Service not starting.** Check the log for the required-flag error:
  `tail -20 ~/Library/Logs/interceptor.log`. As of this version, `interceptor run` refuses to start
  without both `--gateway-url` and a gateway key (`--gateway-key-file` or `INTERCEPTOR_GATEWAY_KEY`);
  the error names exactly which one is missing. See
  [deploy/macos/README.md](deploy/macos/README.md) Part 3 and Part 4.
- **Claude Desktop isn't routing through the interceptor.** Confirm the applied configuration
  actually has `egressProxyUrl` set (deploy/macos/README.md Part 5), then quit Claude Desktop with
  Cmd+Q and reopen it from the Dock — it reads this setting only at startup.
- **Certificate errors** (`TLS handshake with ... failed`, or Claude/curl refusing to connect). The
  CA is not trusted system-wide yet: redo the trust step in deploy/macos/README.md Part 2. For an app
  with its own trust store instead of the keychain, see the environment variables in
  [Quick start](#quick-start).
- **No rows showing up in the gateway.** Check: the key's role is `interceptor`; the gateway has
  ingest enabled (`ingest.enabled` / `GATEWAY_INGEST_ENABLED`); and the interceptor log's `forward
  status: sent=... duplicate=... rejected=... dropped=... spooled=...` line (every 60s) — a nonzero
  `rejected` or `dropped` count, or a growing `spooled` count with the gateway down, explains a gap.

## Reference: reading capture.jsonl

One line per exchange on stdout:

```
14:02:11.482  POST  https://api.anthropic.com/v1/messages  → 200  1.234s  512/9310
```

That is time, method, URL, status, duration, request bytes / response bytes (real totals, not the
truncated capture). With `-v` the headers follow (`>` request, `<` response), then bodies: JSON is
pretty-printed, SSE printed as-is, binary shown as `<N bytes binary>`.

`capture.jsonl` holds one JSON object per exchange (the file is created with mode 0600 because it
contains secrets such as API keys). Fields: `ts`, `mode` (`proxy`, `transparent`,
`transparent-http`, `passthrough`), `client`, `method`, `url`, `req_headers`, `req_body`,
`req_body_truncated`, `req_body_base64`, `status`, `resp_headers`, `resp_body`,
`resp_body_truncated`, `resp_body_base64`, `duration_ms`, `error`, plus `req_bytes` / `resp_bytes`.
Non-UTF-8 bodies are base64 encoded. gzip bodies are decoded for the log only.

```sh
jq -r 'select(.url|test("anthropic")) | .req_body' capture.jsonl
jq -r '[.ts,.method,.url,.status] | @tsv' capture.jsonl
jq -r 'select(.url|endswith("/v1/messages")) | .resp_body' capture.jsonl        # SSE stream text
jq -c 'select(.mode=="passthrough") | {url,req_bytes,resp_bytes}' capture.jsonl
jq -r '.req_headers["Authorization"]?[0] // empty' capture.jsonl               # careful: secrets
```

### Binary protobuf / Connect-RPC

Claude Desktop's chat tab talks Connect-RPC with binary protobuf. Those bodies are not text, so
`req_body` / `resp_body` hold base64. For these content types the record also gets decoded copies:
`application/proto`, `application/x-protobuf`, `application/protobuf` (one message), and
`application/connect+proto`, `application/grpc-web+proto`, `application/grpc-web` (a stream of
envelopes: 1 flag byte + 4-byte big-endian length + payload).

- `req_body_decoded` / `resp_body_decoded`: a JSON tree. Streams become an array, one entry per
  envelope; the end-stream trailer shows as `{"trailer": ...}` and a cut-off capture ends with
  `{"truncated":true}`.
- `decode_error`: set when decoding failed or only part of a truncated body could be decoded.
- The base64 body fields are unchanged. With `-v` the decoded tree is printed instead of
  `<N bytes binary>`.

There is no schema, so **field numbers are the keys** (`{"3":"noodle check here"}` is field 3), and
guesses are involved: a length-delimited value is shown as a string if it is printable UTF-8, else
as a nested message if it parses cleanly, else as `{"bytes":"<base64>","len":N}`. When a value is
both, text wins if it is at least 90% printable ASCII. Varints are numbers (strings above 2^53);
repeated fields are arrays. Fixed32/fixed64 values are shown as unsigned integers, so a `double` or
`float` field appears as its raw bits. Decoding is for display only and never touches traffic.

```sh
jq -c 'select(.resp_body_decoded!=null) | {url,resp_body_decoded}' capture.jsonl
interceptor decode ~/claude-capture.jsonl --grep 'noodle'
interceptor decode --raw 'CgVoZWxsbw=='     # one base64 blob; "-" reads stdin
```

`interceptor decode FILE` works on any capture file, including ones written before this feature: it
decodes from the stored bodies and prints `ts method url` followed by the request and response
trees for every proto record. `--grep REGEX` keeps only records whose decoded (compact JSON) text
matches.

### Streaming responses

Long-lived responses (a chat reply arriving over minutes) would otherwise only appear when the body
ends. For `text/event-stream`, `application/connect+proto`, `application/grpc-web+proto`,
`application/grpc-web`, and NDJSON types without a `Content-Length` (`application/x-ndjson`,
`application/jsonl`, ...), the interceptor also writes **one record per unit as it arrives**:

| Stream | `method` | One record per | Body |
|---|---|---|---|
| SSE | `SSE<` | event (blank-line separated) | raw event text; `resp_body_decoded` = the `data:` payload if it is JSON |
| Connect / gRPC-Web | `RPC<` | envelope | base64 payload; `resp_body_decoded` = protobuf tree, or `{"trailer":...}` |
| NDJSON | `NDJSON<` | line | the line; `resp_body_decoded` if it is JSON |

These records have `mode:"stream"`, the same `url`, `client` and `status`, `ts` = when that unit
arrived, `seq` = 1-based position in the stream, and `duration_ms` = time since the request started.
They carry no headers. Each record is written to the file the moment it is complete (the file is not
buffered), so `tail -f capture.jsonl` and the terminal show a reply live; `-v` prints the decoded unit.
The normal record is still written when the stream ends, with the full body (subject to
`--max-body`), headers, total duration and `stream_units: N`. If the stream is cut by a shutdown or an
upstream error, that final record is written with `error` set and the units seen so far are already
in the file. Splitting happens off the forwarding path, so it never delays the client; if it fails
it stops (logged once) and forwarding continues. Streams with a `Content-Encoding` are not split
(only the final record is written). Use `--no-stream-records` to turn unit records off.

```sh
jq -r 'select(.mode=="stream") | .ts[11:23] + " " + .method + " #" + (.seq|tostring) + " " + (.resp_body_decoded|tostring|.[0:200])' capture.jsonl
```

`interceptor decode` also prints `mode:"stream"` records, and `--grep` matches them.

### WebSocket

The upgrade handshake is logged first (status 101, mode `proxy`). After that the tunnel is relayed
byte for byte while frames are parsed for the log only. WebSocket messages are logged as `WS>`
(client to server, body in `req_body`) and `WS<` (server to client, body in `resp_body`) records
with `mode:"websocket"`, `status:101` and the handshake URL. Fragmented messages are reassembled
into one record, `req_bytes`/`resp_bytes` is the message size on the wire, and `--max-body` applies.
Binary messages are base64. If the server negotiated `permessage-deflate`, the log copy is
decompressed; on failure the raw bytes are logged as base64 with `error` set. Close frames are logged
as text like `close 1000 reason`; ping/pong are shown on stdout only with `-v`. When the tunnel ends a
`WS-CLOSE` record carries the total bytes each way and the tunnel duration.

```sh
jq -r 'select(.mode=="websocket" and (.method|startswith("WS"))) | .method + " " + (.req_body + .resp_body)' capture.jsonl
```

(Plain `.req_body // .resp_body` does not work: an empty string is not null in jq, so it would
always pick `req_body`. Only one of the two is non-empty per record, so concatenating them works.)

Stdout shows `HH:MM:SS.mmm  WS<  wss://host/path  N bytes`; with `-v` the message body follows, JSON
pretty-printed.

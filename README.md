# franken-grpc

[![Docker Hub](https://img.shields.io/docker/v/mohelmrabet/franken-grpc?label=docker.io%2Fmohelmrabet%2Ffranken-grpc&sort=semver)](https://hub.docker.com/r/mohelmrabet/franken-grpc)
[![GHCR](https://img.shields.io/badge/ghcr.io-cleatsquad%2Ffranken--grpc-blue)](https://github.com/CleatSquad/franken-grpc/pkgs/container/franken-grpc)
[![test](https://github.com/CleatSquad/franken-grpc/actions/workflows/test.yml/badge.svg)](https://github.com/CleatSquad/franken-grpc/actions/workflows/test.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A generic gRPC/HTTP2 reverse proxy for PHP backends. It terminates real gRPC
(unary and server-streaming) on one side, and relays every call as a plain
HTTP/1.1 POST to your PHP application on the other — so FrankenPHP,
RoadRunner, Laravel Octane or plain PHP-FPM can serve gRPC clients without
implementing HTTP/2 or protobuf framing themselves.

## Why

PHP has no first-class gRPC server. `franken-grpc` is the small piece that
makes gRPC (streaming included) reachable in front of any PHP backend: it
knows nothing about your `.proto` contracts — it forwards
`/{package}.{Service}/{Method}` calls, routing each one to your backend
(the per-direction message framing differs — see
[How it works](#how-it-works)).

## How it works

```text
gRPC client (HTTP/2)
        │
        ▼
  franken-grpc  (grpc.UnknownServiceHandler — no service registered ahead of time)
        │  HTTP/1.1 POST /{package}.{Service}/{Method}
        ▼
  your PHP backend
```

- Every incoming gRPC call — whatever its service/method name — is relayed
  as an HTTP/1.1 POST to `$BACKEND_URL/{package}.{Service}/{Method}`, body =
  the raw protobuf message.
- Your PHP handler answers with the standard 5-byte gRPC frame on **every**
  response, unary or streaming alike:

  ```text
  1 byte   compression flag — must be 0, this relay does not decompress
  4 bytes  big-endian payload length
  N bytes  raw protobuf payload
  ```

  Several frames in one HTTP response = a server-streaming call; PHP can
  flush each frame as it becomes available for real-time streaming.
- The incoming direction (gRPC client → PHP) is **not** framed: `franken-grpc`
  already decodes the gRPC message before forwarding it, so PHP reads the raw
  protobuf body directly.
- Compression is intentionally unsupported on the PHP↔relay leg (a frame
  declaring a non-zero compression flag is rejected) — native gRPC clients on
  the other side of the relay still get whatever compression `grpc-go`
  negotiates natively, since that segment is real HTTP/2, untouched by this
  restriction.
- Only one piece of gRPC metadata is forwarded to the backend today: the
  `x-user-id` key, sent as an `X-User-Id` HTTP header. Any other metadata a
  client sends is not passed through — there is no generic metadata bridge.
- **Unary vs. streaming is not known to the relay** — that distinction lives
  in your `.proto`, not in anything franken-grpc can see. It relays every
  gRPC frame the PHP backend sends. For a call the client treats as unary,
  gRPC-go's `Invoke()` reads only the *first* frame and silently discards
  the rest — no error. If your backend is meant to answer a "unary" RPC,
  make sure it emits exactly one frame.

## Configuration

| Variable | Description | Default |
|---|---|---|
| `PHP_BACKEND_URL` | Root URL of the PHP backend | `http://app:8080` |
| `GRPC_PORT` | gRPC/HTTP2 listen port | `:9090` |

## Health check

franken-grpc registers the standard [gRPC Health Checking
Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md)
(`grpc.health.v1.Health`) and reports `SERVING` as soon as it starts —
independent of whether the PHP backend is actually reachable, since the
relay has no way to probe it without picking an arbitrary method to call.
Check it with
[`grpc_health_probe`](https://github.com/grpc-ecosystem/grpc-health-probe):

```bash
grpc_health_probe -addr=127.0.0.1:9090
# status: SERVING
```

Note: the relay does not expose the gRPC reflection service, so
`grpcurl` needs an explicit `-proto` for the standard health check
message too — plain `grpcurl -plaintext 127.0.0.1:9090 list` will not
discover this (or any) service.

## Error mapping

A non-`200` HTTP response from the PHP backend becomes a gRPC error, with
the code derived from the HTTP status your backend actually sent — no new
contract to implement, but nothing more precise is possible either (there
is no way today for PHP to signal an exact gRPC code beyond its HTTP
status):

| PHP HTTP status | gRPC code |
|---|---|
| 401 | `Unauthenticated` |
| 403 | `PermissionDenied` |
| 404 | `NotFound` |
| 429 | `ResourceExhausted` |
| other 4xx | `InvalidArgument` |
| anything else (5xx, ...) | `Internal` |

The response body (read up to 1MB) becomes the gRPC error message.

## Image

| Registry | Image | Platforms |
|---|---|---|
| Docker Hub | [`mohelmrabet/franken-grpc`](https://hub.docker.com/r/mohelmrabet/franken-grpc) | `linux/amd64`, `linux/arm64` |
| GHCR | [`ghcr.io/cleatsquad/franken-grpc`](https://github.com/CleatSquad/franken-grpc/pkgs/container/franken-grpc) | `linux/amd64`, `linux/arm64` |

Tags: `latest` tracks the most recent release; pin a version (`1.0.1`, ...)
for anything beyond local testing. Built from `alpine:3.20`, statically
linked (`CGO_ENABLED=0`), ~19 MB.

## Quick start

```bash
docker run -p 9090:9090 \
  -e PHP_BACKEND_URL=http://your-php-backend:8080 \
  mohelmrabet/franken-grpc:latest
```

or from GHCR:

```bash
docker run -p 9090:9090 \
  -e PHP_BACKEND_URL=http://your-php-backend:8080 \
  ghcr.io/cleatsquad/franken-grpc:latest
```

## Framework examples

The framing helper (`frame_grpc_message` below) is a single small class if
you'd rather not hand-roll it:
[`cleatsquad/php-franken-grpc-codec`](https://github.com/CleatSquad/php-franken-grpc-codec)
(`composer require cleatsquad/php-franken-grpc-codec`) — extracted from a
real integration (a Magento module bridging this relay to
`ProductRepositoryInterface`), dependency-free, PHP 8.1+.

### FrankenPHP

```php
// public/index.php
if (str_starts_with($_SERVER['REQUEST_URI'], '/mypackage.MyService/')) {
    $method = substr($_SERVER['REQUEST_URI'], strlen('/mypackage.MyService/'));
    $raw = file_get_contents('php://input');
    // decode $raw as your request message, handle it, then:
    header('Content-Type: application/grpc+proto');
    echo frame_grpc_message($responseBinary); // 1-byte flag + 4-byte length + payload
}
```

### Symfony

Route the `/{package}.{Service}/{Method}` path pattern to a controller that
reads `$request->getContent()` as the raw protobuf body and returns a
framed protobuf response with `Content-Type: application/grpc+proto`.

### Laravel

Same shape via a raw route (`Route::post('/{package}.{service}/{method}', ...)`
with `$request->getContent()`), bypassing Laravel's JSON middleware for that
route — the body is binary protobuf, not JSON.

## Build

```bash
make build   # binary in bin/
make test
make docker-build
```

## License

MIT — see [LICENSE](LICENSE).

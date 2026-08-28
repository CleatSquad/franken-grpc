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

Runnable version, actually tested end-to-end against a real `franken-grpc`
container: [`examples/symfony/`](examples/symfony/).

`/{package}.{Service}/{Method}` isn't a placeholder pattern — it's a literal
path where the first segment happens to contain dots. A single route with a
permissive requirement on that segment covers every method:

```yaml
# config/routes.yaml
grpc_bridge:
    path: /{packageService}/{method}
    controller: App\Controller\GrpcController::handle
    methods: [POST]
    requirements:
        packageService: '.+\..+'
```

```php
use CleatSquad\GrpcFrameCodec\GrpcFrameCodec;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;

class GrpcController
{
    public function __construct(private readonly GrpcFrameCodec $codec) {}

    public function handle(Request $request, string $packageService, string $method): Response
    {
        $requestBytes = $request->getContent(); // raw protobuf, unframed
        $responseBytes = $this->dispatch("$packageService/$method", $requestBytes);

        return new Response($this->codec->encode($responseBytes), 200, [
            'Content-Type' => GrpcFrameCodec::CONTENT_TYPE,
        ]);
    }
}
```

Symfony's autowiring only auto-registers classes under your `App\` service
namespace, so it won't find `GrpcFrameCodec` on its own — register it
explicitly or the container fails with "no such service exists":

```yaml
# config/services.yaml
services:
    App\:
        resource: '../src/'

    CleatSquad\GrpcFrameCodec\GrpcFrameCodec: ~
```

### Laravel

Runnable version, actually tested end-to-end against a real `franken-grpc`
container: [`examples/laravel/`](examples/laravel/).

Same shape, via a raw route with an inline constraint instead of a
requirements block. Two Laravel-specific things to disarm: the `api`
routing group prefixes every path with `/api` by default (pass
`apiPrefix: ''` to `withRouting()` in `bootstrap/app.php`, or the real
path won't match), and the `web` group runs CSRF verification — put this
route on `api`, not `web`, or the CSRF middleware rejects the POST outright.

```php
// bootstrap/app.php
->withRouting(
    web: __DIR__.'/../routes/web.php',
    api: __DIR__.'/../routes/api.php',
    apiPrefix: '', // otherwise every path below is under /api/...
    commands: __DIR__.'/../routes/console.php',
)
```

```php
// routes/api.php
use App\Http\Controllers\GrpcController;

Route::post('/{packageService}/{method}', [GrpcController::class, 'handle'])
    ->where('packageService', '.+\..+');
```

```php
use CleatSquad\GrpcFrameCodec\GrpcFrameCodec;
use Illuminate\Http\Request;

class GrpcController extends Controller
{
    public function __construct(private readonly GrpcFrameCodec $codec) {}

    public function handle(Request $request, string $packageService, string $method)
    {
        $requestBytes = $request->getContent(); // raw protobuf, unframed — not JSON
        $responseBytes = $this->dispatch("$packageService/$method", $requestBytes);

        return response($this->codec->encode($responseBytes))
            ->header('Content-Type', GrpcFrameCodec::CONTENT_TYPE);
    }
}
```

`$request->getContent()` returns the raw body regardless of middleware in
both cases — no JSON-parsing middleware runs on it by default in either
framework — but if your app registers global body-parsing middleware
(a form-data or JSON transformer applied to every request), exclude this
route from it: the body is binary protobuf, parsing it as anything else
will corrupt it.

### Magento 2

Magento's front controller routes on `/{frontName}/{controller}/{action}` —
it cannot express a path containing dots like
`/mypackage.MyService/GetProduct`. A custom `RouterInterface` is needed
instead of `routes.xml`:

```php
// Model/Router.php
use Magento\Framework\App\ActionFactory;
use Magento\Framework\App\ActionInterface;
use Magento\Framework\App\RequestInterface;
use Magento\Framework\App\RouterInterface;

class Router implements RouterInterface
{
    public function __construct(private readonly ActionFactory $actionFactory) {}

    public function match(RequestInterface $request): ?ActionInterface
    {
        if ($request->getPathInfo() !== '/mypackage.MyService/GetProduct') {
            return null;
        }

        return $this->actionFactory->create(\Vendor\Module\Controller\Grpc\GetProduct::class);
    }
}
```

```xml
<!-- etc/frontend/di.xml -->
<type name="Magento\Framework\App\RouterList">
    <arguments>
        <argument name="routerList" xsi:type="array">
            <item name="mymodule_grpc" xsi:type="array">
                <item name="class" xsi:type="string">Vendor\Module\Model\Router</item>
                <item name="sortOrder" xsi:type="string">1</item>
            </item>
        </argument>
    </arguments>
</type>
```

The controller needs `CsrfAwareActionInterface` — Magento's standard
form-key validation on `Action` controllers has no meaning for
machine-to-machine gRPC traffic, and would otherwise redirect every call
with a 302:

```php
use CleatSquad\GrpcFrameCodec\GrpcFrameCodec;
use Magento\Framework\App\Action\Action;
use Magento\Framework\App\Action\HttpPostActionInterface;
use Magento\Framework\App\CsrfAwareActionInterface;
use Magento\Framework\App\Request\InvalidRequestException;
use Magento\Framework\App\RequestInterface;

class GetProduct extends Action implements HttpPostActionInterface, CsrfAwareActionInterface
{
    public function __construct(Context $context, private readonly GrpcFrameCodec $codec)
    {
        parent::__construct($context);
    }

    public function execute()
    {
        $requestBytes = $this->getRequest()->getContent(); // raw protobuf, unframed
        $responseBytes = /* decode, handle, encode your response message */;

        $response = $this->getResponse();
        $response->setHeader('Content-Type', GrpcFrameCodec::CONTENT_TYPE, true);
        $response->setBody($this->codec->encode($responseBytes));

        return $response;
    }

    public function createCsrfValidationException(RequestInterface $request): ?InvalidRequestException
    {
        return null;
    }

    public function validateForCsrf(RequestInterface $request): ?bool
    {
        return true;
    }
}
```

Also watch for Caddy's own defaults if you're serving Magento through
FrankenPHP: `encode zstd br gzip` will silently compress this response and
corrupt the frame (exclude the gRPC-bridge path from it), and
`auto_https` will redirect a plain-HTTP `PHP_BACKEND_URL` call to HTTPS
unless the site also declares an explicit `http://` address.

## Build

```bash
make build   # binary in bin/
make test
make docker-build
```

## License

MIT — see [LICENSE](LICENSE).

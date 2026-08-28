# Examples

Runnable, actually-tested integrations of franken-grpc, one per framework.
Every file here is exactly what was used for the test — nothing has been
simplified or rewritten for the README afterward.

All three share the same tiny contract:

```
examples/proto/echo.proto
```

```proto
service EchoService {
  rpc Echo (EchoRequest) returns (EchoResponse);
}
message EchoRequest  { string message = 1; }
message EchoResponse { string message = 1; string framework = 2; }
```

Each example wires `EchoService/Echo` up in its framework, generates PHP
stubs from this same `.proto` with `protoc --php_out`, and answers a real
`grpcurl` call relayed through a real `franken-grpc` container — the
screenshots below are that call, unedited.

| Example | What it proves |
|---|---|
| [`symfony/`](symfony/) | Custom route on a dotted path, service registered outside `App\` |
| [`laravel/`](laravel/) | `api` routing group, disarming its default `/api` prefix and CSRF |

Magento's example lives as code in the main [README](../README.md#magento-2)
rather than here — the surrounding module boilerplate (`registration.php`,
`module.xml`, DI compilation) doesn't fit a copy-pasteable snippet the way
a single controller does for Symfony/Laravel.

## Reproducing a test yourself

```bash
# 1. generate stubs from examples/proto/echo.proto (see each example's README)
# 2. serve the framework's public/ dir, e.g.:
php -S 0.0.0.0:8080 -t public

# 3. point a franken-grpc container at it
docker run -p 9090:9090 \
  --add-host=host.docker.internal:host-gateway \
  -e PHP_BACKEND_URL=http://host.docker.internal:8080 \
  mohelmrabet/franken-grpc:latest

# 4. call it for real
grpcurl -plaintext -import-path examples/proto -proto echo.proto \
  -d '{"message":"hello"}' 127.0.0.1:9090 echo.EchoService/Echo
```

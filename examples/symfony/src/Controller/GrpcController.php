<?php

declare(strict_types=1);

namespace App\Controller;

use CleatSquad\GrpcFrameCodec\GrpcFrameCodec;
use EchoGrpc\EchoRequest;
use EchoGrpc\EchoResponse;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\Routing\Attribute\Route;

class GrpcController
{
    public function __construct(private readonly GrpcFrameCodec $codec)
    {
    }

    #[Route('/{packageService}/{method}', name: 'grpc_bridge', methods: ['POST'], requirements: ['packageService' => '.+\..+'])]
    public function handle(Request $request, string $packageService, string $method): Response
    {
        $requestBytes = $request->getContent(); // raw protobuf, unframed

        $req = new EchoRequest();
        $req->mergeFromString($requestBytes);

        $resp = new EchoResponse();
        $resp->setMessage($req->getMessage());
        $resp->setFramework('symfony');

        return new Response($this->codec->encode($resp->serializeToString()), 200, [
            'Content-Type' => GrpcFrameCodec::CONTENT_TYPE,
        ]);
    }
}

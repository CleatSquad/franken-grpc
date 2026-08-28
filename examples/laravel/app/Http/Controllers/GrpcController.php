<?php

declare(strict_types=1);

namespace App\Http\Controllers;

use CleatSquad\GrpcFrameCodec\GrpcFrameCodec;
use EchoGrpc\EchoRequest;
use EchoGrpc\EchoResponse;
use Illuminate\Http\Request;

class GrpcController extends Controller
{
    public function __construct(private readonly GrpcFrameCodec $codec)
    {
    }

    public function handle(Request $request, string $packageService, string $method)
    {
        $requestBytes = $request->getContent(); // raw protobuf, unframed — not JSON

        $req = new EchoRequest();
        $req->mergeFromString($requestBytes);

        $resp = new EchoResponse();
        $resp->setMessage($req->getMessage());
        $resp->setFramework('laravel');

        return response($this->codec->encode($resp->serializeToString()))
            ->header('Content-Type', GrpcFrameCodec::CONTENT_TYPE);
    }
}

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	grpcPort         = getEnv("GRPC_PORT", ":9090")
	basePHPBackendURL = strings.TrimRight(getEnv("CONCIO_PHP_BACKEND_URL", "http://app:8080"), "/")
)

// maxFrameSize bounds a single gRPC frame length read from the PHP
// backend's framing header. Without it, a desynced byte stream lets an
// arbitrary 4-byte length field trigger a multi-gigabyte allocation
// (measured: a single bad frame spiked RSS to ~6.9GB before GC reclaimed
// it) — this cap turns that into a clean error instead.
const maxFrameSize = 64 * 1024 * 1024 // 64MB

// maxErrorBodySize bounds how much of a non-200 PHP response is read into
// memory. An error body is normally a short JSON message; this cap only
// guards against an unexpectedly large or slow one.
const maxErrorBodySize = 1 * 1024 * 1024 // 1MB

// httpStatusToGRPCCode maps the PHP backend's HTTP status to a gRPC code
// more precise than a blanket Internal, using only the HTTP status the
// backend already sends today — no new PHP-side contract required. PHP
// cannot yet signal an exact gRPC code beyond its HTTP status (no
// grpc-status/grpc-message trailer bridged through this relay); that
// remains a documented limitation, not something this mapping can close.
func httpStatusToGRPCCode(httpStatus int) codes.Code {
	switch {
	case httpStatus == http.StatusUnauthorized:
		return codes.Unauthenticated
	case httpStatus == http.StatusForbidden:
		return codes.PermissionDenied
	case httpStatus == http.StatusNotFound:
		return codes.NotFound
	case httpStatus == http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case httpStatus >= 400 && httpStatus < 500:
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}

// classifyStreamError turns a resp.Body read failure that occurs mid-stream
// into the gRPC code the caller's context implies (deadline/cancellation),
// falling back to Internal — the same distinction already applied to the
// initial httpClient.Do() failure, extended to errors that happen while
// reading frames.
func classifyStreamError(ctx context.Context, format string, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return status.Error(codes.DeadlineExceeded, "stream deadline exceeded")
	}
	if ctx.Err() == context.Canceled {
		return status.Error(codes.Canceled, "stream canceled by client")
	}
	return status.Errorf(codes.Internal, format, err)
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// No client-side timeout: the request follows whatever deadline the
// incoming gRPC context carries (set by the caller for unary-style calls),
// and stays unbounded when the context has none (long-lived streams).
var httpClient = &http.Client{
	Timeout: 0,
}

// relayToPHP forwards one incoming gRPC message to the PHP backend at
// fullMethod ("/{package}.{Service}/{Method}") and relays every framed
// gRPC message found in the HTTP response back to the caller — one frame
// is exactly what a unary RPC expects, several is a server-stream.
func relayToPHP(ctx context.Context, fullMethod string, rawProtobuf []byte, stream grpc.ServerStream) error {
	reqReceived := time.Now()
	url := basePHPBackendURL + fullMethod
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawProtobuf))
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create stream request: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("Accept", "application/grpc+proto")
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-user-id"); len(vals) > 0 && vals[0] != "" {
			req.Header.Set("X-User-Id", vals[0])
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return status.Error(codes.DeadlineExceeded, "stream deadline exceeded")
		}
		if ctx.Err() == context.Canceled {
			return status.Error(codes.Canceled, "stream canceled by client")
		}
		return status.Errorf(codes.Unavailable, "PHP streaming backend unavailable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Bounded read: an error body is normally a short JSON/text
		// message, but nothing on the wire guarantees that — without a
		// cap, a large or slow error body is the same unbounded-memory
		// risk as an unvalidated frame length.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return status.Errorf(codes.Code(httpStatusToGRPCCode(resp.StatusCode)), "PHP backend returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	header := make([]byte, 5)
	firstByte := true
	msgCount := 0

	for {
		_, err := io.ReadFull(resp.Body, header)
		if err != nil {
			// io.EOF here means zero bytes were read: the body ended
			// cleanly right at a frame boundary — the normal way a
			// response finishes. io.ErrUnexpectedEOF means 1-4 bytes of
			// a header were read before the body ended: a truncated
			// frame, not a clean completion, and must not be treated as
			// one — it would silently drop the last message.
			if err == io.EOF {
				break
			}
			return classifyStreamError(ctx, "failed to read frame header: %v", err)
		}

		if firstByte {
			log.Printf("[%s] First backend byte received after %v", fullMethod, time.Since(reqReceived))
			firstByte = false
		}

		if header[0] != 0 {
			return status.Errorf(codes.Internal, "frame declares compression flag %d, compressed frames are not supported by this relay", header[0])
		}

		length := binary.BigEndian.Uint32(header[1:5])
		if length > maxFrameSize {
			return status.Errorf(codes.Internal, "frame length %d exceeds max %d, backend response desynced", length, maxFrameSize)
		}
		payload := make([]byte, length)
		_, err = io.ReadFull(resp.Body, payload)
		if err != nil {
			return classifyStreamError(ctx, "failed to read frame payload: %v", err)
		}

		msgCount++
		if msgCount == 1 {
			log.Printf("[%s] First gRPC message emitted at %v", fullMethod, time.Since(reqReceived))
		}

		if err := stream.SendMsg(payload); err != nil {
			return status.Errorf(codes.Canceled, "failed to send gRPC stream message: %v", err)
		}
	}

	// A completely empty PHP response (no frame at all) is a legitimate
	// empty result for what was historically a unary call (e.g. an empty
	// session list) — the unary client contract requires exactly one
	// message in return, even with an empty payload. Sending zero
	// messages instead makes the client-side Invoke() fail outright.
	if msgCount == 0 {
		if err := stream.SendMsg([]byte{}); err != nil {
			return status.Errorf(codes.Canceled, "failed to send empty gRPC message: %v", err)
		}
	}

	log.Printf("[%s] Stream completed successfully: %d messages emitted in %v", fullMethod, msgCount, time.Since(reqReceived))
	return nil
}

// genericServiceHandler is registered as grpc.UnknownServiceHandler: it is
// invoked for every incoming call regardless of its service/method name, so
// no Concio-specific ServiceDesc needs to be registered up front. It works
// for both unary calls (the PHP backend returns exactly one gRPC frame) and
// server-streaming calls (several frames), since gRPC-go implements a
// unary RPC as a single-message stream under the hood.
//
// Known limitation: this relay has no static knowledge of which methods are
// unary vs. server-streaming (that distinction lives in the .proto, not in
// anything visible here). If a PHP endpoint that is meant to be unary sends
// more than one frame, grpc-go's unary client helper (Invoke) only reads the
// first one and silently ignores the rest — no error, no crash, just a
// dropped extra message. Fixing this generically would require the relay to
// know the RPC's method contract, which defeats the point of being
// contract-agnostic; it is a PHP-side responsibility to send exactly one
// frame for what is documented as a unary RPC.
func genericServiceHandler(srv interface{}, stream grpc.ServerStream) error {
	fullMethod, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "unable to determine method from stream")
	}

	var in []byte
	if err := stream.RecvMsg(&in); err != nil {
		return status.Errorf(codes.InvalidArgument, "stream recv error: %v", err)
	}

	// Errors returned below only reach the client as a gRPC status; without
	// this log, a failing relay (backend down, deadline, malformed frame)
	// left no trace at all — the "Stream completed successfully" line is
	// never reached on an error path.
	if err := relayToPHP(stream.Context(), fullMethod, in, stream); err != nil {
		log.Printf("[%s] relay failed: %v", fullMethod, err)
		return err
	}
	return nil
}

func main() {
	if !strings.HasPrefix(grpcPort, ":") {
		grpcPort = ":" + grpcPort
	}

	lis, err := net.Listen("tcp", grpcPort)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", grpcPort, err)
	}

	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(genericServiceHandler),
	)

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	go func() {
		log.Printf("concio-franken-grpc-server listening on %s (backend: %s)", grpcPort, basePHPBackendURL)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down concio-franken-grpc-server gracefully...")
	grpcServer.GracefulStop()
}

type rawCodec struct{}

func (rawCodec) Marshal(v interface{}) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	if pm, ok := v.(proto.Message); ok {
		return proto.Marshal(pm)
	}
	return nil, fmt.Errorf("rawCodec: expected []byte or proto.Message, got %T", v)
}

func (rawCodec) Unmarshal(data []byte, v interface{}) error {
	if b, ok := v.(*[]byte); ok {
		*b = make([]byte, len(data))
		copy(*b, data)
		return nil
	}
	if pm, ok := v.(proto.Message); ok {
		return proto.Unmarshal(data, pm)
	}
	return fmt.Errorf("rawCodec: expected *[]byte or proto.Message, got %T", v)
}

func (rawCodec) Name() string {
	return "proto"
}
func (rawCodec) String() string {
	return "proto"
}

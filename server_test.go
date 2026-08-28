package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// frame encodes a raw payload with the standard 5-byte gRPC length-prefix.
func frame(payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

// startTestServer wires the real production grpc.Server — with the generic
// UnknownServiceHandler and no ServiceDesc registered — against fakeBackend,
// over an in-memory bufconn listener. No service name is known in advance,
// exactly like the extracted franken-grpc image would run for any caller.
func startTestServer(t *testing.T, fakeBackend *httptest.Server) *grpc.ClientConn {
	t.Helper()

	origBackend := basePHPBackendURL
	basePHPBackendURL = fakeBackend.URL
	t.Cleanup(func() { basePHPBackendURL = origBackend })

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(genericServiceHandler),
	)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	if err != nil {
		t.Fatalf("dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestGenericHandler_UnaryStyleCall reproduces what SendMessage
// (example.v1.EchoService) did before the hardcoded ServiceDesc was
// removed: one request in, exactly one framed message back.
func TestGenericHandler_UnaryStyleCall(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/example.v1.EchoService/Echo" {
			t.Errorf("unexpected backend path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "request-payload" {
			t.Errorf("unexpected request body: %q", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame([]byte("response-payload")))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo",
		[]byte("request-payload"), &out)
	if err != nil {
		t.Fatalf("unary invoke failed: %v", err)
	}
	if string(out) != "response-payload" {
		t.Fatalf("got %q, want %q", out, "response-payload")
	}
}

// TestGenericHandler_ServerStreamingCall reproduces the ChatStream case:
// several framed messages emitted progressively.
func TestGenericHandler_ServerStreamingCall(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/example.v1.EchoService/StreamEcho" {
			t.Errorf("unexpected backend path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame([]byte("chunk-1")))
		_, _ = w.Write(frame([]byte("chunk-2")))
		_, _ = w.Write(frame([]byte("chunk-3")))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	desc := &grpc.StreamDesc{StreamName: "ChatStream", ServerStreams: true}
	stream, err := conn.NewStream(context.Background(), desc, "/example.v1.EchoService/StreamEcho")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.SendMsg([]byte("request-payload")); err != nil {
		t.Fatalf("send request: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	var chunks [][]byte
	for {
		var msg []byte
		err := stream.RecvMsg(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		chunks = append(chunks, msg)
	}

	want := []string{"chunk-1", "chunk-2", "chunk-3"}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d: %v", len(chunks), len(want), chunks)
	}
	for i, c := range chunks {
		if string(c) != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, c, want[i])
		}
	}
}

// TestGenericHandler_ArbitraryServiceName is the actual regression this
// change targets: a service/method the code has never heard of must be
// relayed without any code change, unlike the previous hardcoded
// ServiceDesc for example.v1.EchoService/AdminService only.
func TestGenericHandler_ArbitraryServiceName(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/some.other.v2.WidgetService/DoThing" {
			t.Errorf("unexpected backend path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame([]byte("ok")))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/some.other.v2.WidgetService/DoThing",
		[]byte("in"), &out)
	if err != nil {
		t.Fatalf("invoke on unregistered service failed: %v", err)
	}
	if string(out) != "ok" {
		t.Fatalf("got %q, want %q", out, "ok")
	}
}

// TestGenericHandler_BackendErrorPropagates keeps the existing HTTP-error
// mapping behavior: a non-200 from the PHP backend must surface as a gRPC
// Internal error, not a silently empty response.
func TestGenericHandler_BackendErrorPropagates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo",
		[]byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_ContextDeadlineHonored verifies the timeout policy is
// now delegated to the caller's context deadline (httpClient has no
// hardcoded 180s timeout anymore) instead of hardcoded on the server.
func TestGenericHandler_ContextDeadlineHonored(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame([]byte("too-late")))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var out []byte
	err := conn.Invoke(ctx, "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got code %v, want DeadlineExceeded (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_OversizedFrameRejected reproduces the measured issue:
// a frame header announcing an implausible length must not trigger a
// multi-gigabyte allocation — it must fail cleanly instead.
func TestGenericHandler_OversizedFrameRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:5], maxFrameSize+1)
		_, _ = w.Write(header)
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_EmptyBackendResponseIsOneEmptyMessage reproduces a real
// production failure: GetSessions with no sessions returns a completely
// empty HTTP body from PHP. A unary client (grpc.Invoke) requires exactly
// one response message, even an empty one — sending zero messages makes
// Invoke fail. Observed live: BFF surfaced "BACKEND_UNREACHABLE".
func TestGenericHandler_EmptyBackendResponseIsOneEmptyMessage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // empty body, no frame at all
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/List", []byte{}, &out)
	if err != nil {
		t.Fatalf("unary invoke on empty backend response failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %q, want empty message", out)
	}
}

// TestGenericHandler_TruncatedFrameHeaderIsError reproduces a backend that
// dies mid-frame: the connection closes after 3 of the required 5 header
// bytes. Before this fix, io.ErrUnexpectedEOF at this point was silently
// treated the same as a clean io.EOF (no more frames), which would have
// dropped the truncated message without ever telling the caller.
func TestGenericHandler_TruncatedFrameHeaderIsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0}) // 3 of 5 header bytes, then nothing
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal for a truncated header (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_TruncatedFramePayloadIsError: header announces a
// payload the backend never fully sends.
func TestGenericHandler_TruncatedFramePayloadIsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:5], 100) // announces 100 bytes
		_, _ = w.Write(header)
		_, _ = w.Write([]byte("only 10 b")) // far fewer than announced
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal for a truncated payload (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_CompressedFrameRejected: the relay has no decompressor
// and must reject a frame that announces compression instead of silently
// forwarding compressed bytes as if they were the raw payload.
func TestGenericHandler_CompressedFrameRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		header := make([]byte, 5)
		header[0] = 1 // compression flag set
		binary.BigEndian.PutUint32(header[1:5], 3)
		_, _ = w.Write(header)
		_, _ = w.Write([]byte("abc"))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal for a compressed frame (err=%v)", status.Code(err), err)
	}
}

// TestGenericHandler_ZeroLengthMessage: a frame with a 0-byte payload is a
// legitimate empty protobuf message (all-default fields), not an error.
func TestGenericHandler_ZeroLengthMessage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame(nil))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if err != nil {
		t.Fatalf("zero-length message should succeed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %q, want empty", out)
	}
}

// TestGenericHandler_HTTPStatusMapping checks that common PHP HTTP error
// statuses map to a more precise gRPC code than a blanket Internal.
func TestGenericHandler_HTTPStatusMapping(t *testing.T) {
	cases := []struct {
		httpStatus int
		wantCode   codes.Code
	}{
		{http.StatusBadRequest, codes.InvalidArgument},
		{http.StatusUnauthorized, codes.Unauthenticated},
		{http.StatusForbidden, codes.PermissionDenied},
		{http.StatusNotFound, codes.NotFound},
		{http.StatusTooManyRequests, codes.ResourceExhausted},
		{http.StatusInternalServerError, codes.Internal},
		{http.StatusBadGateway, codes.Internal},
	}

	for _, tc := range cases {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.httpStatus)
			_, _ = w.Write([]byte("error body"))
		}))

		conn := startTestServer(t, backend)

		var out []byte
		err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
		if status.Code(err) != tc.wantCode {
			t.Errorf("HTTP %d: got code %v, want %v (err=%v)", tc.httpStatus, status.Code(err), tc.wantCode, err)
		}
		backend.Close()
	}
}

// TestGenericHandler_LargeErrorBodyIsBounded ensures a pathological error
// body doesn't force an unbounded read into memory.
func TestGenericHandler_LargeErrorBodyIsBounded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(make([]byte, maxErrorBodySize*2))
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	var out []byte
	err := conn.Invoke(context.Background(), "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got code %v, want Internal (err=%v)", status.Code(err), err)
	}
	// The error message itself must not carry the full 2x body.
	if len(err.Error()) > maxErrorBodySize+1024 {
		t.Fatalf("error message is %d bytes, expected it bounded near maxErrorBodySize", len(err.Error()))
	}
}

// TestGenericHandler_DeadlineExceededMidStream verifies that a deadline
// expiring while frames are still being read maps to DeadlineExceeded, not
// a generic Internal — exercising classifyStreamError on the payload-read
// path rather than only on the initial httpClient.Do() call.
func TestGenericHandler_DeadlineExceededMidStream(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:5], 5)
		_, _ = w.Write(header)
		_, _ = w.Write([]byte("abc")) // 3 of 5 announced payload bytes
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond) // outlast the client deadline below
	}))
	defer backend.Close()

	conn := startTestServer(t, backend)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var out []byte
	err := conn.Invoke(ctx, "/example.v1.EchoService/Echo", []byte("in"), &out)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got code %v, want DeadlineExceeded (err=%v)", status.Code(err), err)
	}
}

var _ = bytes.MinRead // keep bytes import if unused paths change

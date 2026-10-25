package adapters

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codefly-base/pkg/gen"
	"codefly-base/pkg/gen/genconnect"
	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// paddedRequest is a VersionRequest of about size bytes: one unknown
// length-delimited field (15), which proto3 skips without complaint, so its
// size is the only thing a bound can refuse it for.
func paddedRequest(size int) []byte {
	return append(paddingHeader(size), make([]byte, size)...)
}

func paddingHeader(size int) []byte {
	return binary.AppendUvarint([]byte{0x7a}, uint64(size))
}

func paddedMessage(t *testing.T, size int) *gen.VersionRequest {
	t.Helper()
	message := &gen.VersionRequest{}
	if err := proto.Unmarshal(paddedRequest(size), message); err != nil {
		t.Fatalf("build padded request: %v", err)
	}
	return message
}

// endlessBody is a request body of undeclared length that counts what the
// listener reads of it: a padded VersionRequest of size bytes, produced only as
// fast as it is read. It hides its length (it is no *bytes.Reader), so the
// request goes out chunked, the case the listener used to buffer whole.
type endlessBody struct {
	header    []byte
	remaining int64
	read      atomic.Int64
}

func newEndlessBody(size int) *endlessBody {
	return &endlessBody{header: paddingHeader(size), remaining: int64(size)}
}

func (b *endlessBody) Read(p []byte) (int, error) {
	n := copy(p, b.header)
	b.header = b.header[n:]
	if n == 0 {
		if b.remaining == 0 {
			return 0, io.EOF
		}
		n = len(p)
		if int64(n) > b.remaining {
			n = int(b.remaining)
		}
		clear(p[:n])
		b.remaining -= int64(n)
	}
	b.read.Add(int64(n))
	return n, nil
}

// postConnect sends body to Version as a Connect unary call with
// application/proto and returns the Connect error code the listener answered
// with ("" when it served the call), and its message.
func postConnect(t *testing.T, client *http.Client, base string, body io.Reader, contentLength int64) (string, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, base+"/api.WebService/Version", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.ContentLength = contentLength
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return "", ""
	}
	var refusal struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode refusal (HTTP %d): %v", response.StatusCode, err)
	}
	return refusal.Code, refusal.Message
}

// readCeiling is how much of an oversize body the listener may consume beyond
// the bound before it refuses: socket buffers and the server's post-response
// drain, never the body itself.
const readCeiling = 16 << 20

// TestConnectListenerRefusesAnOversizeRequestBeforeReadingIt is the regression
// for the Connect face buffering a request of undeclared length up to 4 GiB
// before any bound or interceptor ran: the transcoder has to measure a Connect
// unary body to frame it for gRPC, and with vanguard's default buffer limit it
// read all of it first. It is held to Configuration.MaxMessageBytes now, so an
// oversize request is refused as resource_exhausted after the bound, not after
// the body — declared or undeclared, in process or dialled.
func TestConnectListenerRefusesAnOversizeRequestBeforeReadingIt(t *testing.T) {
	const bound = 64 << 10
	const offered = 256 << 20
	_, dialOption := startFrontDoor(t)
	for _, face := range []struct {
		name   string
		config *Configuration
	}{
		{name: "in process", config: &Configuration{MaxMessageBytes: bound}},
		{name: "dialled", config: &Configuration{MaxMessageBytes: bound, GatewayDialOptions: []grpc.DialOption{dialOption}}},
	} {
		t.Run(face.name, func(t *testing.T) {
			base, client := startConfiguredService(t, face.config)

			undeclared := newEndlessBody(offered)
			code, message := postConnect(t, client, base, undeclared, -1)
			if code != "resource_exhausted" {
				t.Fatalf("undeclared oversize request answered %q (%s), want resource_exhausted", code, message)
			}
			if read := undeclared.read.Load(); read > bound+readCeiling {
				t.Fatalf("the listener read %d bytes of an undeclared %d-byte request before refusing it over a %d-byte bound", read, offered, bound)
			}

			declared := newEndlessBody(offered)
			code, message = postConnect(t, client, base, declared, int64(offered+len(paddingHeader(offered))))
			if code != "resource_exhausted" {
				t.Fatalf("declared oversize request answered %q (%s), want resource_exhausted", code, message)
			}
			if read := declared.read.Load(); read > readCeiling {
				t.Fatalf("the listener read %d bytes of a request whose declared length was already over the bound", read)
			}

			if code, message := postConnect(t, client, base, strings.NewReader(string(paddedRequest(bound/2))), -1); code != "" {
				t.Fatalf("a request under the bound answered %q (%s), want served", code, message)
			}
		})
	}
}

// TestConnectListenerKeepsGRPCsDefaultBound: a service that sets no
// MaxMessageBytes keeps gRPC's own 4 MiB receive bound, and the Connect face
// holds to that number rather than to vanguard's 4 GiB.
func TestConnectListenerKeepsGRPCsDefaultBound(t *testing.T) {
	base, client := startConfiguredService(t, &Configuration{})
	if code, message := postConnect(t, client, base, newEndlessBody(5<<20), -1); code != "resource_exhausted" {
		t.Fatalf("a 5 MiB request answered %q (%s), want resource_exhausted at gRPC's 4 MiB default", code, message)
	}
	if code, message := postConnect(t, client, base, strings.NewReader(string(paddedRequest(defaultMaxMessageBytes-16))), -1); code != "" {
		t.Fatalf("a request just under 4 MiB answered %q (%s), want served", code, message)
	}
}

// faceClients starts a service under config and returns a Connect and a gRPC
// client of it.
func faceClients(t *testing.T, config *Configuration) (genconnect.WebServiceClient, gen.WebServiceClient) {
	t.Helper()
	base, httpClient := startConfiguredService(t, config)
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", config.EndpointGrpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return genconnect.NewWebServiceClient(httpClient, base), gen.NewWebServiceClient(conn)
}

// TestMessageBoundRaisesEveryFace: raising MaxMessageBytes raises it on the
// gRPC face and the Connect face alike, and a request over it is refused on
// both.
func TestMessageBoundRaisesEveryFace(t *testing.T) {
	const bound = 8 << 20
	connectClient, grpcClient := faceClients(t, &Configuration{MaxMessageBytes: bound})

	under, over := paddedMessage(t, 5<<20), paddedMessage(t, bound+(1<<20))
	if _, err := connectClient.Version(context.Background(), connect.NewRequest(under)); err != nil {
		t.Fatalf("Connect: a 5 MiB request under the raised bound was refused: %v", err)
	}
	if _, err := grpcClient.Version(context.Background(), under); err != nil {
		t.Fatalf("gRPC: a 5 MiB request under the raised bound was refused: %v", err)
	}
	if _, err := connectClient.Version(context.Background(), connect.NewRequest(over)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("Connect: a request over the bound answered %v, want resource_exhausted", err)
	}
	if _, err := grpcClient.Version(context.Background(), over); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC: a request over MaxMessageBytes answered %v, want ResourceExhausted", err)
	}
}

// TestServiceReceiveBoundIsNeverOverridden is the regression for the
// generated default overriding a receive bound the service raised itself: a
// service that appended grpc.MaxRecvMsgSize to GRPCServerOptions (the way
// services did before MaxMessageBytes existed) had it silently replaced by
// the 4 MiB default, because grpc-go applies server options in order and the
// generated bound came last. The service's own option must stay in force on
// the gRPC face, whether MaxMessageBytes is unset or set lower.
func TestServiceReceiveBoundIsNeverOverridden(t *testing.T) {
	const serviceBound = 32 << 20
	for _, face := range []struct {
		name   string
		config *Configuration
	}{
		{name: "MaxMessageBytes unset", config: &Configuration{GRPCServerOptions: []grpc.ServerOption{grpc.MaxRecvMsgSize(serviceBound)}}},
		{name: "MaxMessageBytes lower", config: &Configuration{MaxMessageBytes: 1 << 20, GRPCServerOptions: []grpc.ServerOption{grpc.MaxRecvMsgSize(serviceBound)}}},
	} {
		t.Run(face.name, func(t *testing.T) {
			_, grpcClient := faceClients(t, face.config)
			if _, err := grpcClient.Version(context.Background(), paddedMessage(t, 6<<20)); err != nil {
				t.Fatalf("gRPC: a 6 MiB request under the service's own %d-byte receive bound was refused: %v", serviceBound, err)
			}
			if _, err := grpcClient.Version(context.Background(), paddedMessage(t, serviceBound+(1<<20))); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("gRPC: a request over the service's own receive bound answered %v, want ResourceExhausted", err)
			}
		})
	}
}

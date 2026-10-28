package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	_ "google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/durationpb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"
)

// Use a reflected protobuf contract to exercise the real generated bridge
// without introducing generated fixture code or requiring a proto toolchain.
var jsonProfileDescriptor = sync.OnceValues(func() (protoreflect.MessageDescriptor, error) {
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: &kind}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	fields := []*descriptorpb.FieldDescriptorProto{
		field("child", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".profile.Response"),
		field("created_at", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".google.protobuf.Timestamp"),
		field("display_name", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
		field("signed_count", 4, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
		field("unsigned_count", 5, descriptorpb.FieldDescriptorProto_TYPE_UINT64, ""),
		field("kind", 6, descriptorpb.FieldDescriptorProto_TYPE_ENUM, ".profile.Kind"),
		field("value", 7, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".google.protobuf.Value"),
		field("wrapped", 8, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".google.protobuf.Int32Value"),
		field("labels", 9, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
		field("attributes", 10, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".profile.Response.AttributesEntry"),
		field("delay", 11, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".google.protobuf.Duration"),
		field("payload", 12, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".google.protobuf.Any"),
		field("custom_name", 13, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
	}
	fields[8].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fields[9].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fields[12].JsonName = proto.String("publicName")
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("connect_json_profile_test.proto"), Package: proto.String("profile"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto", "google/protobuf/struct.proto", "google/protobuf/wrappers.proto", "google/protobuf/duration.proto", "google/protobuf/any.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("Kind"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("UNSPECIFIED"), Number: proto.Int32(0)}, {Name: proto.String("READY"), Number: proto.Int32(1)},
		}}},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Response"), Field: fields,
			NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("AttributesEntry"), Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)}, Field: []*descriptorpb.FieldDescriptorProto{
				field("key", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""), field("value", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
			}}},
		}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Wire"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Echo"), InputType: proto.String(".profile.Response"), OutputType: proto.String(".profile.Response")}}}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		return nil, err
	}
	if err := protoregistry.GlobalFiles.RegisterFile(file); err != nil {
		return nil, err
	}
	return file.Messages().Get(0), nil
})

// TestConnectJSONUsesTheOmittedDefaultProfile checks raw response keys: typed
// protobuf decoding would erase the difference between an absent object and
// null. Every case traverses NewConnectServer and a real gRPC method.
func TestConnectJSONUsesTheOmittedDefaultProfile(t *testing.T) {
	for _, shape := range []string{"in-process", "dialled", "connect-only-dialled"} {
		t.Run(shape, func(t *testing.T) {
			url, client := startJSONProfileBridge(t, shape)
			for _, tc := range []struct{ name, request, response string }{
				{"absent", `{}`, `{}`},
				{"empty-parent", `{"child":{}}`, `{"child":{}}`},
				{"nested-unset", `{"child":{"displayName":"nested"}}`, `{"child":{"displayName":"nested"}}`},
				{"defaults", `{"displayName":"","signedCount":"0","unsignedCount":"0","kind":"UNSPECIFIED","labels":[],"attributes":{}}`, `{}`},
				{"null-value-zero-wrapper", `{"value":null,"wrapped":0}`, `{"value":null,"wrapped":0}`},
				{"unknown-input", `{"unknownField":true}`, `{}`},
				{"populated", `{"displayName":"public","publicName":"custom","signedCount":"-9223372036854775808","unsignedCount":"18446744073709551615","kind":"READY","createdAt":"2026-10-28T12:00:00Z","delay":"1.500s","payload":{"@type":"type.googleapis.com/google.protobuf.StringValue","value":"resolved"},"labels":["a"],"attributes":{"k":"v"},"child":{"wrapped":7}}`, `{"displayName":"public","publicName":"custom","signedCount":"-9223372036854775808","unsignedCount":"18446744073709551615","kind":"READY","createdAt":"2026-10-28T12:00:00Z","delay":"1.500s","payload":{"@type":"type.googleapis.com/google.protobuf.StringValue","value":"resolved"},"labels":["a"],"attributes":{"k":"v"},"child":{"wrapped":7}}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/profile.Wire/Echo", strings.NewReader(tc.request))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Connect-Protocol-Version", "1")
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
					}
					var got, want any
					if err := json.Unmarshal(body, &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(tc.response), &want); err != nil {
						t.Fatal(err)
					}
					gotBytes, _ := json.Marshal(got)
					wantBytes, _ := json.Marshal(want)
					if string(gotBytes) != string(wantBytes) {
						t.Fatalf("raw Connect JSON = %s, want %s", body, tc.response)
					}
				})
			}
		})
	}
}

func startJSONProfileBridge(t *testing.T, shape string) (string, *http.Client) {
	t.Helper()
	descriptor, err := jsonProfileDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	t.Cleanup(server.Stop)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "profile.Wire", HandlerType: (*interface{})(nil), Metadata: "connect_json_profile_test.proto",
		Methods: []grpc.MethodDesc{{MethodName: "Echo", Handler: func(_ any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			request := dynamicpb.NewMessage(descriptor)
			if err := decode(request); err != nil {
				return nil, err
			}
			return request, nil
		}}},
	}, struct{}{})
	face := httptest.NewUnstartedServer(nil)
	port := uint16(face.Listener.Addr().(*net.TCPAddr).Port)
	c := &Configuration{EndpointConnectHost: "127.0.0.1", EndpointConnectPort: &port}
	if shape != "in-process" {
		c.GatewayDialOptions = []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4 << 20))}
	}
	if shape == "dialled" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c.EndpointGrpcHost = "127.0.0.1"
		c.EndpointGrpcPort = uint16(listener.Addr().(*net.TCPAddr).Port)
		go func() { _ = server.Serve(listener) }()
	}
	bridge, err := NewConnectServer(c, server)
	if err != nil {
		face.Close()
		t.Fatal(err)
	}
	face.Config.Handler = bridge.handler
	face.Start()
	t.Cleanup(face.Close)
	return face.URL, &http.Client{Timeout: 5 * time.Second}
}

// Registration is process-global. A subprocess proves a real upstream JSON
// codec cannot bypass the Connect profile without contaminating other tests.
func TestConnectJSONProfileCannotBeBypassedByARegisteredJSONCodec(t *testing.T) {
	const child = "CODEFLY_TEST_REGISTERED_JSON_CODEC"
	if os.Getenv(child) != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConnectJSONProfileCannotBeBypassedByARegisteredJSONCodec$", "-test.v")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("registered JSON codec subprocess: %v\n%s", err, output)
		}
		return
	}
	codec := &emittingJSONCodec{}
	encoding.RegisterCodec(codec)
	TestConnectJSONUsesTheOmittedDefaultProfile(t)
	if calls := codec.calls.Load(); calls != 0 {
		t.Errorf("Connect used upstream JSON codec %d times; want protobuf target", calls)
	}
}

type emittingJSONCodec struct{ calls atomic.Int64 }

func (*emittingJSONCodec) Name() string { return "json" }
func (c *emittingJSONCodec) Marshal(value any) ([]byte, error) {
	c.calls.Add(1)
	message, ok := value.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("not a protobuf message: %T", value)
	}
	return protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(message)
}
func (c *emittingJSONCodec) Unmarshal(data []byte, value any) error {
	c.calls.Add(1)
	message, ok := value.(proto.Message)
	if !ok {
		return fmt.Errorf("not a protobuf message: %T", value)
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, message)
}

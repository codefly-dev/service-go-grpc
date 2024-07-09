package adapters

/* -----------------------------------------------------------------

⚠️ This code is generated by the agent. Do not edit this file!

----------------------------------------------------------------- */

import (
	"base_replacement/pkg/gen"
	"bytes"
	"context"
	"fmt"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/status"
	"io"
	"net/http"

	"github.com/codefly-dev/core/wool"

	"google.golang.org/grpc/metadata"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type RestServer struct {
	config *Configuration
}

func NewRestServer(c *Configuration) (*RestServer, error) {
	server := &RestServer{config: c}
	// Start Rest server (and proxy calls to gRPC server endpoint)
	return server, nil
}

func CustomHeaderToGRPCMetadataAnnotator(ctx context.Context, req *http.Request) metadata.MD {
	return wool.MetadataFromRequest(ctx, req)
}

func customErrorHandler(ctx context.Context, mux *runtime.ServeMux, marshaler runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	// Check if it's a "not found" error
	if runtime.HTTPStatusFromCode(status.Code(err)) == http.StatusNotFound {
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}

	// For other errors, use the default error handler
	runtime.DefaultHTTPErrorHandler(ctx, mux, marshaler, w, r, err)
}

func (s *RestServer) Run(ctx context.Context) error {
	fmt.Println("Starting Rest server at", *s.config.EndpointHttpPort)

	// Create a CORS handler
	c := Cors()

	gwMux := runtime.NewServeMux(
		runtime.WithMetadata(CustomHeaderToGRPCMetadataAnnotator),
		runtime.WithErrorHandler(customErrorHandler))

	// Register generated gateway handlers

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	err := gen.RegisterWebServiceHandlerFromEndpoint(ctx, gwMux, fmt.Sprintf("0.0.0.0:%d", s.config.EndpointGrpcPort), opts)
	if err != nil {
		return err
	}

	// Wrap your mux with the CORS handler
	handler := c.Handler(gwMux)

	return http.ListenAndServe(fmt.Sprintf(":%d", *s.config.EndpointHttpPort), logRequestBody(handler))
}

type logResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rsp *logResponseWriter) WriteHeader(code int) {
	rsp.statusCode = code
	rsp.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the original http.ResponseWriter. This is necessary
// to expose Flush() and Push() on the underlying response writer.
func (rsp *logResponseWriter) Unwrap() http.ResponseWriter {
	return rsp.ResponseWriter
}

func newLogResponseWriter(w http.ResponseWriter) *logResponseWriter {
	return &logResponseWriter{w, http.StatusOK}
}

// logRequestBody logs the request body when the response status code is not 200.
func logRequestBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := newLogResponseWriter(w)

		// Note that buffering the entire request body could consume a lot of memory.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
			return
		}
		clonedR := r.Clone(r.Context())
		clonedR.Body = io.NopCloser(bytes.NewReader(body))

		h.ServeHTTP(lw, clonedR)

		if lw.statusCode != http.StatusOK {
			grpclog.Errorf("http error %+v request body %+v", lw.statusCode, string(body))
		}
	})
}

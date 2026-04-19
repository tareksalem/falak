// Package grpc provides the gRPC transport layer for the Falak API.
// It delegates all business logic to api/core.Core and handles only
// marshaling, authentication, and server lifecycle.
package grpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/tareksalem/falak/api/core"
	falakhttp "github.com/tareksalem/falak/api/http"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

// Server is the top-level API server. It multiplexes gRPC and HTTP on
// a single port via content-type sniffing (gRPC uses HTTP/2 with
// application/grpc content-type).
type Server struct {
	core       *core.Core
	grpcServer *grpc.Server
	httpMux    *http.ServeMux
	logger     *zap.Logger
	addr       string

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithAddr sets the listen address (e.g. ":9090").
func WithAddr(addr string) ServerOption {
	return func(s *Server) { s.addr = addr }
}

// WithServerLogger sets the logger.
func WithServerLogger(logger *zap.Logger) ServerOption {
	return func(s *Server) { s.logger = logger }
}

// WithGRPCOptions passes additional gRPC server options (interceptors, etc).
func WithGRPCOptions(opts ...grpc.ServerOption) ServerOption {
	return func(s *Server) {
		s.grpcServer = grpc.NewServer(opts...)
	}
}

// NewServer creates a new API server backed by the given Core.
func NewServer(c *core.Core, opts ...ServerOption) *Server {
	s := &Server{
		core:    c,
		logger:  zap.NewNop(),
		addr:    ":9090",
		httpMux: http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.grpcServer == nil {
		s.grpcServer = grpc.NewServer()
	}

	// Register gRPC services.
	pb.RegisterCapsuleServiceServer(s.grpcServer, &capsuleService{core: c})
	pb.RegisterClusterServiceServer(s.grpcServer, &clusterService{core: c})
	pb.RegisterSystemServiceServer(s.grpcServer, &systemService{core: c})

	// Enable gRPC reflection for debugging (grpcurl, grpcui).
	reflection.Register(s.grpcServer)

	// Register HTTP health endpoints.
	s.httpMux.HandleFunc("/healthz", s.handleHealthz)
	s.httpMux.HandleFunc("/readyz", s.handleReadyz)

	// Register SSE streaming endpoints for browser/curl clients.
	sse := falakhttp.NewSSEHandler(c, s.logger.Named("sse"))
	sse.RegisterRoutes(s.httpMux)

	return s
}

// Start begins serving on the configured address. It multiplexes gRPC
// and HTTP on the same port by inspecting the content-type header.
func (s *Server) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("api server: listen on %s: %w", s.addr, err)
	}

	s.logger.Info("api server starting",
		zap.String("addr", s.addr))

	// Use a simple HTTP server that routes gRPC vs HTTP based on
	// content type. HTTP/2 connections with application/grpc go to
	// the gRPC server; everything else goes to the HTTP mux.
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && r.Header.Get("Content-Type") == "application/grpc" {
				s.grpcServer.ServeHTTP(w, r)
			} else {
				s.httpMux.ServeHTTP(w, r)
			}
		}),
	}

	go func() {
		if err := httpServer.Serve(lis); err != nil && err != http.ErrServerClosed {
			s.logger.Error("api server error", zap.Error(err))
		}
	}()

	// Graceful shutdown on context cancellation.
	go func() {
		<-s.ctx.Done()
		s.logger.Info("api server draining")
		s.grpcServer.GracefulStop()
		httpServer.Close()
		s.logger.Info("api server stopped")
	}()

	return nil
}

// Stop shuts down the server gracefully.
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string {
	return s.addr
}

// --- HTTP health handlers ------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.core.Healthz(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.core.Readyz(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

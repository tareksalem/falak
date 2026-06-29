// Package grpc provides the gRPC transport layer for the Falak API.
// It delegates all business logic to api/core.Core and handles only
// marshaling, authentication, and server lifecycle.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"

	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/tareksalem/falak/api/core"
	falakhttp "github.com/tareksalem/falak/api/http"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

// DefaultMaxPortRetries is how many ports the API server walks forward
// when its requested port is already bound. Set to 0 to disable the
// retry behavior (strict-bind mode for production).
const DefaultMaxPortRetries = 20

// Server is the top-level API server. It multiplexes gRPC and HTTP on
// a single port via content-type sniffing (gRPC uses HTTP/2 with
// application/grpc content-type).
type Server struct {
	core            *core.Core
	grpcServer      *grpc.Server
	httpMux         *http.ServeMux
	logger          *zap.Logger
	addr            string
	maxPortRetries  int

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

// WithMaxPortRetries overrides DefaultMaxPortRetries. Pass 0 to disable
// the auto port-shift behavior (strict-bind: fail immediately if the
// requested port is in use). The default is suitable for dev/test; set
// to 0 for production deployments where a load balancer expects a fixed
// port.
func WithMaxPortRetries(n int) ServerOption {
	return func(s *Server) { s.maxPortRetries = n }
}

// NewServer creates a new API server backed by the given Core.
func NewServer(c *core.Core, opts ...ServerOption) *Server {
	s := &Server{
		core:           c,
		logger:         zap.NewNop(),
		addr:           ":9090",
		httpMux:        http.NewServeMux(),
		maxPortRetries: DefaultMaxPortRetries,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.grpcServer == nil {
		s.grpcServer = grpc.NewServer()
	}

	// Build service implementations once — shared between gRPC server
	// and the in-process grpc-gateway mux.
	capsuleSrv := &capsuleService{core: c}
	clusterSrv := &clusterService{core: c}
	systemSrv := &systemService{core: c}
	serviceSrv := &serviceService{core: c}
	nodeSrv := &nodeService{core: c}

	// Register gRPC services.
	pb.RegisterCapsuleServiceServer(s.grpcServer, capsuleSrv)
	pb.RegisterClusterServiceServer(s.grpcServer, clusterSrv)
	pb.RegisterSystemServiceServer(s.grpcServer, systemSrv)
	pb.RegisterServiceServiceServer(s.grpcServer, serviceSrv)
	pb.RegisterNodeServiceServer(s.grpcServer, nodeSrv)

	// Enable gRPC reflection for debugging (grpcurl, grpcui).
	reflection.Register(s.grpcServer)

	// Build grpc-gateway mux that dispatches REST → in-process server
	// implementations. NOTE: the in-process dispatch path bypasses the
	// gRPC interceptor chain, so HTTP requests do NOT yet enforce the
	// same mTLS / auth guarantees as the gRPC path. This is a documented
	// follow-up from 11B.20 — see docs/api.md ("HTTP mTLS gap"). The
	// Service endpoints inherit that gap, just like the capsule
	// endpoints already do; closing it is a single-PR change that
	// applies an HTTP middleware around gwmux for the production wiring.
	gwmux := gwruntime.NewServeMux()
	if err := pb.RegisterCapsuleServiceHandlerServer(context.Background(), gwmux, capsuleSrv); err != nil {
		panic(fmt.Sprintf("register capsule gateway: %v", err))
	}
	if err := pb.RegisterClusterServiceHandlerServer(context.Background(), gwmux, clusterSrv); err != nil {
		panic(fmt.Sprintf("register cluster gateway: %v", err))
	}
	if err := pb.RegisterSystemServiceHandlerServer(context.Background(), gwmux, systemSrv); err != nil {
		panic(fmt.Sprintf("register system gateway: %v", err))
	}
	if err := pb.RegisterServiceServiceHandlerServer(context.Background(), gwmux, serviceSrv); err != nil {
		panic(fmt.Sprintf("register service gateway: %v", err))
	}
	if err := pb.RegisterNodeServiceHandlerServer(context.Background(), gwmux, nodeSrv); err != nil {
		panic(fmt.Sprintf("register node gateway: %v", err))
	}

	// Register HTTP health endpoints.
	s.httpMux.HandleFunc("/healthz", s.handleHealthz)
	s.httpMux.HandleFunc("/readyz", s.handleReadyz)

	// SSE streaming endpoints (exact paths win over the prefix handler
	// below, so /v1alpha1/watch keeps routing to SSE).
	sse := falakhttp.NewSSEHandler(c, s.logger.Named("sse"))
	sse.RegisterRoutes(s.httpMux)

	// Mount grpc-gateway as the catch-all for /v1alpha1/* REST routes.
	s.httpMux.Handle("/v1alpha1/", gwmux)

	return s
}

// Start begins serving on the configured address. It multiplexes gRPC
// and HTTP on the same port by inspecting the content-type header.
//
// If the requested port is already in use and maxPortRetries > 0, the
// server walks the port forward (Vite-style) up to maxPortRetries times
// before giving up. Server.Addr() returns the actually-bound address.
func (s *Server) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	lis, resolvedAddr, err := listenWithRetry(s.addr, s.maxPortRetries, s.logger)
	if err != nil {
		return err
	}
	s.addr = resolvedAddr

	s.logger.Info("api server starting",
		zap.String("addr", s.addr))

	// Multiplex gRPC and HTTP on a single port. Without TLS we need
	// H2C (HTTP/2 cleartext) so gRPC clients can negotiate HTTP/2 —
	// otherwise the standard net/http server only accepts HTTP/1.1
	// and gRPC clients see "frame too large" on the server preface.
	muxHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && r.Header.Get("Content-Type") == "application/grpc" {
			s.grpcServer.ServeHTTP(w, r)
		} else {
			s.httpMux.ServeHTTP(w, r)
		}
	})
	httpServer := &http.Server{
		Handler: h2c.NewHandler(muxHandler, &http2.Server{}),
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

// listenWithRetry binds the requested addr. When the port is already in
// use it walks forward up to maxRetries additional ports, returning the
// listener bound to the first free port. A non-EADDRINUSE error fails
// immediately. When the port shifts, a WARN log records both the
// requested and the actually-bound addresses so operators see it.
//
// addr forms not in host:port shape (e.g. unix sockets, or a port the
// stdlib can't parse) bypass the retry and call net.Listen verbatim.
func listenWithRetry(addr string, maxRetries int, logger *zap.Logger) (net.Listener, string, error) {
	host, portStr, splitErr := net.SplitHostPort(addr)
	port, parseErr := strconv.Atoi(portStr)
	if splitErr != nil || parseErr != nil || port == 0 || maxRetries <= 0 {
		// Either unparseable, port 0 (kernel-assigned), or retries
		// disabled — single attempt, propagate the raw error.
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, addr, fmt.Errorf("api server: listen on %s: %w", addr, err)
		}
		return lis, addr, nil
	}

	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		tryAddr := net.JoinHostPort(host, strconv.Itoa(port+i))
		lis, err := net.Listen("tcp", tryAddr)
		if err == nil {
			if i > 0 && logger != nil {
				logger.Warn("api port shifted (requested port in use)",
					zap.String("requested", addr),
					zap.String("bound", tryAddr),
					zap.Int("offset", i))
			}
			return lis, tryAddr, nil
		}
		if !isAddrInUseErr(err) {
			return nil, addr, fmt.Errorf("api server: listen on %s: %w", tryAddr, err)
		}
		lastErr = err
	}
	return nil, addr, fmt.Errorf("api server: no free port in range %d-%d on %s: %w",
		port, port+maxRetries, host, lastErr)
}

// isAddrInUseErr reports whether err is the "address already in use"
// syscall error wrapped by the net package.
func isAddrInUseErr(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

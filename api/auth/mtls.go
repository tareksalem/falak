// Package auth provides authentication middleware for the Falak API.
// The primary mechanism is mTLS: clients present a cluster-signed
// certificate, and the server validates it against the cluster root CA.
// An --insecure mode is available for local development.
package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// MTLSConfig configures mTLS for the API server.
type MTLSConfig struct {
	// CACertPath is the path to the cluster CA certificate PEM file.
	CACertPath string

	// ServerCertPath is the path to the server's certificate PEM file.
	ServerCertPath string

	// ServerKeyPath is the path to the server's private key PEM file.
	ServerKeyPath string

	// Insecure disables TLS entirely. Only for local development.
	// When true, CACertPath/ServerCertPath/ServerKeyPath are ignored.
	Insecure bool

	Logger *zap.Logger
}

// NewTLSConfig builds a tls.Config for the API server from the MTLS
// configuration. Returns nil if Insecure is true.
func NewTLSConfig(cfg MTLSConfig) (*tls.Config, error) {
	if cfg.Insecure {
		if cfg.Logger != nil {
			cfg.Logger.Warn("API server running in INSECURE mode — no TLS, no client auth")
		}
		return nil, nil
	}

	// Load server certificate + key.
	cert, err := tls.LoadX509KeyPair(cfg.ServerCertPath, cfg.ServerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("auth: load server cert: %w", err)
	}

	// Load CA certificate for client verification.
	caCert, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("auth: load CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("auth: failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// GRPCServerOptions returns gRPC server options for mTLS. If insecure,
// returns no TLS options (plain TCP).
func GRPCServerOptions(cfg MTLSConfig) ([]grpc.ServerOption, error) {
	tlsCfg, err := NewTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		// Insecure mode — no TLS.
		return nil, nil
	}
	creds := credentials.NewTLS(tlsCfg)
	return []grpc.ServerOption{grpc.Creds(creds)}, nil
}

// UnaryInterceptor returns a gRPC unary interceptor that verifies the
// client presented a valid mTLS certificate. In insecure mode, it's a
// no-op passthrough.
func UnaryInterceptor(insecure bool) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if insecure {
			return handler(ctx, req)
		}
		if err := verifyPeer(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a gRPC stream interceptor for mTLS verification.
func StreamInterceptor(insecure bool) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if insecure {
			return handler(srv, ss)
		}
		if err := verifyPeer(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// HTTPMiddleware wraps an HTTP handler with mTLS verification.
// In insecure mode, it's a passthrough.
func HTTPMiddleware(insecure bool, next http.Handler) http.Handler {
	if insecure {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, `{"error":"client certificate required"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// verifyPeer checks that the gRPC context has a verified TLS peer.
func verifyPeer(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer info in context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "no TLS info — client certificate required")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 {
		return status.Error(codes.Unauthenticated, "client certificate not verified")
	}
	return nil
}

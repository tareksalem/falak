# Falak API

The Falak control plane exposes a gRPC service multiplexed with an
HTTP/REST gateway on a single TCP port (default `:9090`). Content-type
sniffing routes HTTP/2 + `application/grpc` traffic to the gRPC server
and everything else to the HTTP mux.

Services exposed under `falak.api.v1alpha1`:

| Service        | Purpose                                 |
| -------------- | --------------------------------------- |
| CapsuleService | Capsule lifecycle CRUD + log streaming  |
| ClusterService | Join / leave / list cluster operations  |
| NodeService    | Node inventory and health               |
| SystemService  | Node info, version, health probes       |
| ServiceService | Falak Service (traffic management) CRUD |

## HTTP mTLS gap (follow-up from 11B.20)

The current grpc-gateway wiring in `api/grpc/server.go` dispatches HTTP
requests directly to the in-process service implementations. This
bypass keeps the loop-back dial out of the hot path but it also
**skips the gRPC interceptor chain** — meaning mTLS verification and
any other auth middleware configured for the gRPC server does not
apply to HTTP traffic on `/v1alpha1/*`.

Same gap exists for the long-standing capsule + cluster + system
endpoints; ServiceService inherits it as of 11B.20. Closing it is a
single follow-up change:

1. Pull the existing `api/auth/` HTTP middleware (it already verifies
   client cert chains using the cluster CA).
2. Wrap the `gwmux` handler before mounting it on `s.httpMux` under
   `/v1alpha1/`. The HTTP server then enforces the same mTLS guarantee
   as the gRPC server.
3. Update integration tests to drive both surfaces with the same client
   certificate fixture.

Until that lands, operators should treat the HTTP gateway as
unauthenticated and only expose it on trusted networks. The gRPC
surface remains mTLS-protected and is the recommended path for any
production tooling.

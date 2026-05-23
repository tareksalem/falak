module github.com/tareksalem/falak/config

go 1.25.7

require (
	cuelang.org/go v0.16.0
	github.com/tareksalem/falak/capsule v0.0.0
	github.com/tareksalem/falak/service v0.0.0
)

replace (
	github.com/tareksalem/falak/capsule => ../capsule
	github.com/tareksalem/falak/service => ../service
	github.com/tareksalem/falak/shared => ../shared
)

require (
	github.com/cockroachdb/apd/v3 v3.2.1 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect
	github.com/emicklei/proto v1.14.3 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/ipfs/go-cid v0.5.0 // indirect
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/libp2p/go-buffer-pool v0.1.0 // indirect
	github.com/libp2p/go-libp2p v0.48.0 // indirect
	github.com/libp2p/go-libp2p-pubsub v0.15.0 // indirect
	github.com/libp2p/go-msgio v0.3.0 // indirect
	github.com/mattn/go-sqlite3 v1.14.41 // indirect
	github.com/minio/sha256-simd v1.0.1 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	github.com/multiformats/go-base32 v0.1.0 // indirect
	github.com/multiformats/go-base36 v0.2.0 // indirect
	github.com/multiformats/go-multiaddr v0.16.0 // indirect
	github.com/multiformats/go-multiaddr-fmt v0.1.0 // indirect
	github.com/multiformats/go-multibase v0.2.0 // indirect
	github.com/multiformats/go-multicodec v0.9.1 // indirect
	github.com/multiformats/go-multihash v0.2.3 // indirect
	github.com/multiformats/go-multistream v0.6.1 // indirect
	github.com/multiformats/go-varint v0.0.7 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/protocolbuffers/txtpbfmt v0.0.0-20260217160748-a481f6a22f94 // indirect
	github.com/qmuntal/stateless v1.8.0 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	github.com/tareksalem/falak/shared v0.0.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/exp v0.0.0-20250606033433-dcc06ee1d476 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/telemetry v0.0.0-20260311193753-579e4da9a98c // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	lukechampine.com/blake3 v1.4.1 // indirect
)

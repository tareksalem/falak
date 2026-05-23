module github.com/tareksalem/falak/network

go 1.25.7

require (
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/mattn/go-sqlite3 v1.14.41
	github.com/miekg/dns v1.1.72
	github.com/stretchr/testify v1.11.1
	github.com/tareksalem/falak/shared v0.0.0
	github.com/vishvananda/netlink v1.3.1
	go.uber.org/zap v1.27.1
	golang.org/x/sys v0.42.0
	google.golang.org/protobuf v1.36.11
)

replace github.com/tareksalem/falak/shared => ../shared

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/mod v0.34.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/tools v0.43.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

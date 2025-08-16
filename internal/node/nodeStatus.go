package node

type NodeStatus string

const (
	initializing NodeStatus = "initializing"
	ready        NodeStatus = "ready"
	shuttingDown NodeStatus = "shuttingDown"
	shutdown     NodeStatus = "shutdown"
	verifying    NodeStatus = "verifying"
	verified     NodeStatus = "verified"
)

var NodeStatusEnum = struct {
	Initializing NodeStatus
	Ready        NodeStatus
	ShuttingDown NodeStatus
	Shutdown     NodeStatus
	Verifying    NodeStatus
	Verified     NodeStatus
}{
	Initializing: initializing,
	Ready:        ready,
	ShuttingDown: shuttingDown,
	Shutdown:     shutdown,
	Verifying:    verifying,
	Verified:     verified,
}

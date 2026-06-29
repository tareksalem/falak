// Package node — APIFacade is the adapter that lets api/core talk to a
// running Node without importing it. It implements core.NodeFacade so
// the API server (api/grpc) can be constructed with `core.New(facade)`.
//
// Translation rules:
//   - Cluster + node read paths come from the phonebook.
//   - Capsule operations route through capsule.Manager.
//   - Watch streams subscribe to the internal event bus and forward
//     resource-typed events as core.WatchEvent.
package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tareksalem/falak/api/core"
	"github.com/tareksalem/falak/capsule"
	capsuleenums "github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// APIFacade adapts *Node to the api/core.NodeFacade interface.
type APIFacade struct {
	node *Node
}

// NewAPIFacade constructs an adapter for the given running node.
func NewAPIFacade(n *Node) *APIFacade { return &APIFacade{node: n} }

// --- identity ------------------------------------------------------------

// NodeID returns the local peer ID as a string.
func (a *APIFacade) NodeID() string { return a.node.ID().String() }

// NodeName returns the operator-provided node name.
func (a *APIFacade) NodeName() string { return a.node.Name() }

// ListenAddrs returns the libp2p listen multiaddrs as strings.
func (a *APIFacade) ListenAddrs() []string { return a.node.ListenAddrs() }

// IsReady reports whether the node has finished startup.
func (a *APIFacade) IsReady() bool {
	state := a.node.State()
	return state == NodeStateEnum.Running() || state == NodeStateEnum.Draining()
}

// StartedAt returns the wall-clock time the node finished Start.
func (a *APIFacade) StartedAt() time.Time { return a.node.StartedAt() }

// --- cluster operations --------------------------------------------------

// ClusterJoin joins the node to a cluster via the underlying Node.Join.
func (a *APIFacade) ClusterJoin(ctx context.Context, req core.JoinClusterRequest) error {
	cfg := ClusterConfig{
		Path:           req.Path,
		PSK:            []byte(req.PSK),
		BootstrapPeers: req.BootstrapPeers,
	}
	return a.node.Join(ctx, cfg)
}

// ClusterLeave detaches from a cluster.
func (a *APIFacade) ClusterLeave(_ context.Context, path string) error {
	return a.node.Leave(path)
}

// ClusterList returns one entry per joined cluster, with the live member count.
func (a *APIFacade) ClusterList(_ context.Context) (*core.ListClustersResponse, error) {
	joined := a.node.JoinedClustersWithTime()
	out := &core.ListClustersResponse{
		Clusters: make([]core.ClusterResource, 0, len(joined)),
	}
	pb := a.node.Phonebook()
	for path, joinedAt := range joined {
		count := 0
		if pb != nil {
			if c, err := pb.CountByCluster(path); err == nil {
				count = c
			}
		}
		out.Clusters = append(out.Clusters, core.ClusterResource{
			Path:     path,
			JoinedAt: joinedAt,
			Members:  count,
		})
	}
	return out, nil
}

// --- node operations -----------------------------------------------------

// NodeList returns every node the phonebook tracks for the requested cluster.
func (a *APIFacade) NodeList(_ context.Context, req core.ListNodesRequest) (*core.ListNodesResponse, error) {
	if req.Cluster == "" {
		return nil, fmt.Errorf("%w: cluster is required", core.ErrInvalidArgument)
	}
	pb := a.node.Phonebook()
	if pb == nil {
		return &core.ListNodesResponse{}, nil
	}
	entries, err := pb.GetByCluster(req.Cluster)
	if err != nil {
		return nil, fmt.Errorf("phonebook: %w", err)
	}
	out := &core.ListNodesResponse{
		Nodes: make([]core.NodeResource, 0, len(entries)),
	}
	for _, e := range entries {
		out.Nodes = append(out.Nodes, phonebookEntryToNodeResource(e, req.Cluster))
	}
	return out, nil
}

// NodeGet returns a single phonebook entry as a NodeResource.
func (a *APIFacade) NodeGet(_ context.Context, cluster, nodeID string) (*core.NodeResource, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("%w: node id is required", core.ErrInvalidArgument)
	}
	pb := a.node.Phonebook()
	if pb == nil {
		return nil, fmt.Errorf("%w: phonebook unavailable", core.ErrUnavailable)
	}
	if cluster == "" {
		matches, err := pb.GetByNode(nodeID)
		if err != nil {
			return nil, fmt.Errorf("phonebook: %w", err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%w: node %s", core.ErrNotFound, nodeID)
		}
		r := phonebookEntryToNodeResource(matches[0], matches[0].ClusterPath)
		return &r, nil
	}
	e, err := pb.Get(nodeID, cluster)
	if err != nil {
		return nil, fmt.Errorf("phonebook: %w", err)
	}
	if e == nil {
		return nil, fmt.Errorf("%w: node %s in cluster %s", core.ErrNotFound, nodeID, cluster)
	}
	r := phonebookEntryToNodeResource(e, cluster)
	return &r, nil
}

func phonebookEntryToNodeResource(e *phonebook.Entry, cluster string) core.NodeResource {
	var caps phonebook.Capabilities
	if e.Capabilities != nil {
		caps = *e.Capabilities
	}
	name := e.Name
	if name == "" {
		// Older agents or peers that never published a name — fall
		// back to the peer ID so the CLI never shows a blank column.
		name = e.NodeID
	}
	return core.NodeResource{
		Meta: core.ObjectMeta{
			ID:        e.NodeID,
			Name:      name,
			Cluster:   cluster,
			CreatedAt: e.FirstSeen,
			UpdatedAt: e.UpdatedAt,
		},
		Status: core.NodeStatusView{
			Status:           string(e.Status),
			Addresses:        e.Addresses,
			CPUCores:         caps.CPUCores,
			MemoryMB:         caps.MemoryMB,
			Datacenter:       e.Datacenter,
			Region:           e.Region,
			LastProbeTime:    e.LastProbeTime,
			LastProbeSuccess: e.LastProbeSuccess,
			ReliabilityScore: e.ReliabilityScore,
		},
	}
}

// --- capsule operations --------------------------------------------------

// CapsuleCreate translates the API request into a capsule.CapsuleSpec
// and calls capsule.Manager.Create. Only the minimum fields needed to
// boot a workload are mapped; advanced fields (placement, scaling,
// momentum) round-trip via the CUE file path.
func (a *APIFacade) CapsuleCreate(ctx context.Context, cluster string, req core.CreateCapsuleRequest) (*core.CapsuleResource, error) {
	mgr := a.node.Capsules()
	if mgr == nil {
		return nil, fmt.Errorf("%w: capsule manager not initialized", core.ErrUnavailable)
	}
	if cluster == "" {
		cluster = req.Cluster
	}
	if cluster == "" {
		return nil, fmt.Errorf("%w: cluster is required", core.ErrInvalidArgument)
	}

	spec := capsule.CapsuleSpec{
		Name:  req.Name,
		Image: req.Image,
		Orbit: req.Orbit,
		Labels: capsule.Labels(req.Labels),
		Resources: capsule.ResourceRequirements{
			CPUCores:    req.CPUCores,
			CPUCoresMax: req.CPUCoresMax,
			MemoryMB:    req.MemoryMB,
			MemoryMBMax: req.MemoryMBMax,
			DiskMB:      req.DiskMB,
		},
		Replicas: capsule.ReplicaConfig{
			Min:   req.ReplicasMin,
			Max:   req.ReplicasMax,
			Exact: req.ReplicasExact,
		},
		Runtime: capsule.RuntimeConfig{
			Env: req.Env,
		},
		Command: req.Command,
	}
	if req.Tier != "" {
		spec.Tier = capsuleenums.Tier(req.Tier)
	}
	if len(req.Ports) > 0 {
		spec.Runtime.Network.Ports = make([]capsule.PortMapping, 0, len(req.Ports))
		for _, p := range req.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			cp := uint16(p.Container)
			hp := uint16(p.Host)
			spec.Runtime.Network.Ports = append(spec.Runtime.Network.Ports, capsule.PortMapping{
				Name:          p.Name,
				ContainerPort: cp,
				HostPort:      hp,
				Protocol:      proto,
			})
		}
	}

	c, err := mgr.Create(ctx, cluster, spec)
	if err != nil {
		// Translate domain-layer sentinels into API-layer sentinels so
		// the gRPC interceptor returns the right code. Without this the
		// core wraps the error with ErrInternal and the client sees a
		// useless "internal error" instead of "AlreadyExists".
		if errors.Is(err, capsule.ErrCapsuleNameConflict) {
			return nil, fmt.Errorf("%w: %v", core.ErrAlreadyExists, err)
		}
		return nil, err
	}
	r := capsuleToResource(c)
	return &r, nil
}

// CapsuleGet returns one capsule by ID.
func (a *APIFacade) CapsuleGet(_ context.Context, id string) (*core.CapsuleResource, error) {
	mgr := a.node.Capsules()
	if mgr == nil {
		return nil, fmt.Errorf("%w: capsule manager not initialized", core.ErrUnavailable)
	}
	c := mgr.Get(capsule.CapsuleID(id))
	if c == nil {
		return nil, fmt.Errorf("%w: capsule %s", core.ErrNotFound, id)
	}
	r := capsuleToResource(c)
	return &r, nil
}

// CapsuleList enumerates the capsule store.
func (a *APIFacade) CapsuleList(_ context.Context, req core.ListCapsulesRequest) (*core.ListCapsulesResponse, error) {
	mgr := a.node.Capsules()
	if mgr == nil {
		return &core.ListCapsulesResponse{}, nil
	}
	all := mgr.ListSnapshot()
	out := &core.ListCapsulesResponse{
		Capsules: make([]core.CapsuleResource, 0, len(all)),
	}
	for _, c := range all {
		if req.Orbit != "" && c.Spec.Orbit != req.Orbit {
			continue
		}
		if req.Cluster != "" && c.ClusterID != req.Cluster {
			continue
		}
		if len(req.Labels) > 0 && !labelsContain(c.Spec.Labels, req.Labels) {
			continue
		}
		out.Capsules = append(out.Capsules, capsuleToResource(c))
	}
	return out, nil
}

// CapsuleDelete removes a capsule from the local store and gossip mesh.
func (a *APIFacade) CapsuleDelete(ctx context.Context, id string) error {
	mgr := a.node.Capsules()
	if mgr == nil {
		return fmt.Errorf("%w: capsule manager not initialized", core.ErrUnavailable)
	}
	return mgr.Delete(ctx, capsule.CapsuleID(id))
}

// CapsuleUpdate is the limited in-place spec swap.
func (a *APIFacade) CapsuleUpdate(ctx context.Context, req core.UpdateCapsuleRequest) (*core.CapsuleResource, error) {
	mgr := a.node.Capsules()
	if mgr == nil {
		return nil, fmt.Errorf("%w: capsule manager not initialized", core.ErrUnavailable)
	}
	existing := mgr.Get(capsule.CapsuleID(req.ID))
	if existing == nil {
		return nil, fmt.Errorf("%w: capsule %s", core.ErrNotFound, req.ID)
	}
	spec := existing.Spec
	if req.Image != "" {
		spec.Image = req.Image
	}
	if len(req.Env) > 0 {
		if spec.Runtime.Env == nil {
			spec.Runtime.Env = map[string]string{}
		}
		for k, v := range req.Env {
			spec.Runtime.Env[k] = v
		}
	}
	if len(req.Command) > 0 {
		spec.Command = req.Command
	}
	c, err := mgr.Update(ctx, capsule.CapsuleID(req.ID), spec)
	if err != nil {
		return nil, err
	}
	r := capsuleToResource(c)
	return &r, nil
}

func labelsContain(have capsule.Labels, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func capsuleToResource(c *capsule.Capsule) core.CapsuleResource {
	replicas := make([]core.ReplicaView, 0, len(c.Replicas))
	for _, r := range c.Replicas {
		replicas = append(replicas, core.ReplicaView{
			ReplicaID: string(r.ReplicaID),
			NodeID:    r.NodeID,
			Status:    string(r.Status),
			StartedAt: r.StartedAt,
		})
	}
	return core.CapsuleResource{
		Meta: core.ObjectMeta{
			ID:        string(c.ID),
			Name:      c.Spec.Name,
			Cluster:   c.ClusterID,
			Labels:    map[string]string(c.Spec.Labels),
			CreatedAt: c.CreatedAt,
			UpdatedAt: c.UpdatedAt,
		},
		Spec: core.CapsuleSpecView{
			Image:       c.Spec.Image,
			ImageDigest: c.Spec.ImageDigest,
			Orbit:       c.Spec.Orbit,
			Tier:        string(c.Spec.Tier),
			CPUCores:    c.Spec.Resources.CPUCores,
			MemoryMB:    c.Spec.Resources.MemoryMB,
			DiskMB:      c.Spec.Resources.DiskMB,
			ReplicasMin: c.Spec.Replicas.Min,
			ReplicasMax: c.Spec.Replicas.Max,
			Env:         c.Spec.Runtime.Env,
			Command:     c.Spec.Command,
			NetworkMode: string(c.Spec.Runtime.Network.Mode),
			Ports:       portMappingsToCore(c.Spec.Runtime.Network.Ports),
		},
		Status: core.CapsuleStatusView{
			Status:   string(c.Status),
			Replicas: replicas,
		},
	}
}

// portMappingsToCore lifts capsule.PortMapping into the API-layer form so
// CapsuleSpecView can carry Ports back through the gRPC wire. The capsule
// type uses uint16 for ports; the API surface uses int32 to match the
// proto wire shape.
func portMappingsToCore(in []capsule.PortMapping) []core.PortMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]core.PortMapping, 0, len(in))
	for _, p := range in {
		out = append(out, core.PortMapping{
			Name:      p.Name,
			Container: int32(p.ContainerPort),
			Host:      int32(p.HostPort),
			Protocol:  p.Protocol,
		})
	}
	return out
}

// --- watch stream --------------------------------------------------------

// WatchEvents bridges the internal rxgo event bus to a typed core.WatchEvent
// channel filtered by resourceType (capsule | node | "" for all).
func (a *APIFacade) WatchEvents(ctx context.Context, resourceType string) (<-chan core.WatchEvent, error) {
	bus := a.node.EventBus()
	if bus == nil {
		return nil, fmt.Errorf("%w: event bus unavailable", core.ErrUnavailable)
	}
	topics := watchTopics(resourceType)
	if len(topics) == 0 {
		return nil, fmt.Errorf("%w: unknown resource type %q", core.ErrInvalidArgument, resourceType)
	}

	out := make(chan core.WatchEvent, 64)
	chans := make([]<-chan events.Event, 0, len(topics))
	for _, t := range topics {
		chans = append(chans, bus.Subscribe(t))
	}

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			for _, ch := range chans {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						continue
					}
					we := core.WatchEvent{
						Type:      classifyWatchEvent(ev.EventType()),
						Resource:  ev,
						Timestamp: time.Now(),
					}
					select {
					case out <- we:
					case <-ctx.Done():
						return
					}
				default:
				}
			}
			// brief pacer so the default branches don't spin
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func watchTopics(resourceType string) []string {
	switch strings.ToLower(resourceType) {
	case "", "all":
		return []string{
			events.TypeCapsuleCreated,
			events.TypeCapsuleAnnounced,
			events.TypeCapsuleReceived,
			events.TypeCapsuleAssigned,
			events.TypeCapsuleRunning,
			events.TypeCapsuleStopping,
			events.TypeCapsuleStopped,
			events.TypeCapsuleWithdrawn,
			events.TypeNodeSuspected,
			events.TypeNodeQuarantined,
			events.TypeNodeFailed,
			events.TypeNodeRecovered,
		}
	case "capsule", "capsules":
		return []string{
			events.TypeCapsuleCreated,
			events.TypeCapsuleAnnounced,
			events.TypeCapsuleReceived,
			events.TypeCapsuleAssigned,
			events.TypeCapsuleRunning,
			events.TypeCapsuleStopping,
			events.TypeCapsuleStopped,
			events.TypeCapsuleWithdrawn,
		}
	case "node", "nodes":
		return []string{
			events.TypeNodeSuspected,
			events.TypeNodeQuarantined,
			events.TypeNodeFailed,
			events.TypeNodeRecovered,
		}
	}
	return nil
}

func classifyWatchEvent(eventType string) core.WatchEventType {
	switch {
	case strings.HasSuffix(eventType, ".created"),
		strings.HasSuffix(eventType, ".announced"),
		strings.HasSuffix(eventType, ".received"),
		strings.HasSuffix(eventType, ".assigned"),
		strings.HasSuffix(eventType, ".running"):
		return core.WatchEventTypeEnum.Added()
	case strings.HasSuffix(eventType, ".stopping"),
		strings.HasSuffix(eventType, ".stopped"),
		strings.HasSuffix(eventType, ".withdrawn"),
		strings.HasSuffix(eventType, ".failed"),
		strings.HasSuffix(eventType, ".quarantined"):
		return core.WatchEventTypeEnum.Deleted()
	case strings.HasSuffix(eventType, ".suspected"),
		strings.HasSuffix(eventType, ".recovered"):
		return core.WatchEventTypeEnum.Modified()
	default:
		return core.WatchEventTypeEnum.Modified()
	}
}

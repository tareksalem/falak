package grpc

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/api/core"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

// nodeService implements the dedicated NodeService gRPC API. Closes
// Bug #6 — the project previously routed node listing through
// ClusterService.Members, which mixed cluster admin and node introspection
// in one RPC. This separation lets future node-only operations (drain,
// live stats, eviction) land on a clean surface.
type nodeService struct {
	pb.UnimplementedNodeServiceServer
	core *core.Core
}

func (s *nodeService) List(ctx context.Context, req *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	if req.Cluster == "" {
		return nil, core.ToGRPCError(core.WrapInvalidArgument("cluster is required"))
	}
	result, err := s.core.ListNodes(ctx, core.ListNodesRequest{Cluster: req.Cluster})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	resp := &pb.ListNodesResponse{}
	for _, n := range result.Nodes {
		resp.Nodes = append(resp.Nodes, nodeResourceToProto(n))
	}
	return resp, nil
}

func (s *nodeService) Get(ctx context.Context, req *pb.GetNodeRequest) (*pb.NodeInfo, error) {
	n, err := s.lookup(ctx, req)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Health returns the same payload as Get today; reserved for diverging
// once richer health (live stats, probe history) lands without breaking
// the simpler Get RPC.
func (s *nodeService) Health(ctx context.Context, req *pb.GetNodeRequest) (*pb.NodeInfo, error) {
	return s.lookup(ctx, req)
}

// lookup resolves a node by ID or operator-assigned name across the
// requested cluster (or every joined cluster when Cluster is empty).
func (s *nodeService) lookup(ctx context.Context, req *pb.GetNodeRequest) (*pb.NodeInfo, error) {
	if req.Id == "" {
		return nil, core.ToGRPCError(core.WrapInvalidArgument("node id is required"))
	}
	clusters := []string{req.Cluster}
	if req.Cluster == "" {
		// Fan out across every joined cluster — same behavior the CLI
		// already had via resolveClusters.
		list, err := s.core.ListClusters(ctx)
		if err != nil {
			return nil, core.ToGRPCError(err)
		}
		clusters = clusters[:0]
		for _, c := range list.Clusters {
			clusters = append(clusters, c.Path)
		}
	}
	for _, cluster := range clusters {
		nodes, err := s.core.ListNodes(ctx, core.ListNodesRequest{Cluster: cluster})
		if err != nil {
			continue
		}
		for _, n := range nodes.Nodes {
			if n.Meta.ID == req.Id || n.Meta.Name == req.Id {
				return nodeResourceToProto(n), nil
			}
		}
	}
	return nil, core.ToGRPCError(core.WrapNotFound(fmt.Sprintf("node %q", req.Id)))
}

// nodeResourceToProto mirrors clusterService.Members' projection so both
// RPCs return identical payloads. Kept as a helper so future health
// fields land on both surfaces together.
func nodeResourceToProto(n core.NodeResource) *pb.NodeInfo {
	info := &pb.NodeInfo{
		Id:               n.Meta.ID,
		Name:             n.Meta.Name,
		Status:           n.Status.Status,
		Addresses:        n.Status.Addresses,
		Datacenter:       n.Status.Datacenter,
		Region:           n.Status.Region,
		CpuCores:         n.Status.CPUCores,
		MemoryMb:         n.Status.MemoryMB,
		LastProbeSuccess: n.Status.LastProbeSuccess,
		ReliabilityScore: n.Status.ReliabilityScore,
	}
	if !n.Status.LastProbeTime.IsZero() {
		info.LastProbeTime = timestamppb.New(n.Status.LastProbeTime)
	}
	return info
}

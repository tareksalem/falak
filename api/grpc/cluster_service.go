package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/api/core"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

type clusterService struct {
	pb.UnimplementedClusterServiceServer
	core *core.Core
}

func (s *clusterService) Join(ctx context.Context, req *pb.JoinClusterRequest) (*emptypb.Empty, error) {
	err := s.core.JoinCluster(ctx, core.JoinClusterRequest{
		Path:           req.Path,
		PSK:            req.Psk,
		BootstrapPeers: req.BootstrapPeers,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *clusterService) Leave(ctx context.Context, req *pb.LeaveClusterRequest) (*emptypb.Empty, error) {
	if err := s.core.LeaveCluster(ctx, req.Path); err != nil {
		return nil, core.ToGRPCError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *clusterService) List(ctx context.Context, _ *pb.ListClustersRequest) (*pb.ListClustersResponse, error) {
	result, err := s.core.ListClusters(ctx)
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	resp := &pb.ListClustersResponse{}
	for _, c := range result.Clusters {
		resp.Clusters = append(resp.Clusters, &pb.ClusterInfo{
			Path:     c.Path,
			JoinedAt: timestamppb.New(c.JoinedAt),
			Members:  int32(c.Members),
		})
	}
	return resp, nil
}

func (s *clusterService) Members(ctx context.Context, req *pb.ClusterMembersRequest) (*pb.ClusterMembersResponse, error) {
	nodes, err := s.core.ListNodes(ctx, core.ListNodesRequest{Cluster: req.Path})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	resp := &pb.ClusterMembersResponse{}
	for _, n := range nodes.Nodes {
		resp.Nodes = append(resp.Nodes, &pb.NodeInfo{
			Id:         n.Meta.ID,
			Name:       n.Meta.Name,
			Status:     n.Status.Status,
			Addresses:  n.Status.Addresses,
			Datacenter: n.Status.Datacenter,
			Region:     n.Status.Region,
			CpuCores:   n.Status.CPUCores,
			MemoryMb:   n.Status.MemoryMB,
		})
	}
	return resp, nil
}

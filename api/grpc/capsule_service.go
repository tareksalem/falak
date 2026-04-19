package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/api/core"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

// capsuleService implements pb.CapsuleServiceServer by delegating to Core.
type capsuleService struct {
	pb.UnimplementedCapsuleServiceServer
	core *core.Core
}

func (s *capsuleService) Create(ctx context.Context, req *pb.CreateCapsuleRequest) (*pb.CapsuleResource, error) {
	result, err := s.core.CreateCapsule(ctx, core.CreateCapsuleRequest{
		Cluster:          req.Cluster,
		Name:             req.Name,
		Image:            req.Image,
		Orbit:            req.Orbit,
		Tier:             req.Tier,
		Labels:           req.Labels,
		CPUCores:         req.CpuCores,
		CPUCoresMax:      req.CpuCoresMax,
		MemoryMB:         req.MemoryMb,
		MemoryMBMax:      req.MemoryMbMax,
		DiskMB:           req.DiskMb,
		ReplicasMin:      req.ReplicasMin,
		ReplicasMax:      req.ReplicasMax,
		ReplicasExact:    req.ReplicasExact,
		Env:              req.Env,
		Command:          req.Command,
		NetworkMode:      req.NetworkMode,
		ImageDigest:      req.ImageDigest,
		RegistryURL:      req.RegistryUrl,
		RegistryUsername:  req.RegistryUsername,
		RegistryPassword:  req.RegistryPassword,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return capsuleToProto(result), nil
}

func (s *capsuleService) Get(ctx context.Context, req *pb.GetCapsuleRequest) (*pb.CapsuleResource, error) {
	result, err := s.core.GetCapsule(ctx, core.GetCapsuleRequest{
		ID:      req.Id,
		Cluster: req.Cluster,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return capsuleToProto(result), nil
}

func (s *capsuleService) List(ctx context.Context, req *pb.ListCapsulesRequest) (*pb.ListCapsulesResponse, error) {
	var pag core.Pagination
	if req.Pagination != nil {
		pag.PageSize = req.Pagination.PageSize
		pag.NextPageToken = req.Pagination.NextPageToken
	}

	result, err := s.core.ListCapsules(ctx, core.ListCapsulesRequest{
		Cluster:    req.Cluster,
		Labels:     req.Labels,
		Orbit:      req.Orbit,
		Pagination: pag,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}

	resp := &pb.ListCapsulesResponse{}
	for _, c := range result.Capsules {
		resp.Capsules = append(resp.Capsules, capsuleToProto(&c))
	}
	resp.Paging = &pb.PagedResult{
		NextPageToken: result.Paging.NextPageToken,
		TotalCount:    result.Paging.TotalCount,
	}
	return resp, nil
}

func (s *capsuleService) Update(ctx context.Context, req *pb.UpdateCapsuleRequest) (*pb.CapsuleResource, error) {
	result, err := s.core.UpdateCapsule(ctx, core.UpdateCapsuleRequest{
		ID:      req.Id,
		Cluster: req.Cluster,
		Image:   req.Image,
		Env:     req.Env,
		Command: req.Command,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return capsuleToProto(result), nil
}

func (s *capsuleService) Delete(ctx context.Context, req *pb.DeleteCapsuleRequest) (*emptypb.Empty, error) {
	if err := s.core.DeleteCapsule(ctx, core.DeleteCapsuleRequest{
		ID:      req.Id,
		Cluster: req.Cluster,
	}); err != nil {
		return nil, core.ToGRPCError(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *capsuleService) Watch(req *pb.WatchCapsulesRequest, stream pb.CapsuleService_WatchServer) error {
	ch, err := s.core.Watch(stream.Context(), "capsule")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for ev := range ch {
		pbEv := &pb.WatchEvent{
			Timestamp: timestamppb.New(ev.Timestamp),
		}
		switch ev.Type {
		case core.WatchEventTypeEnum.Added():
			pbEv.Type = pb.WatchEvent_ADDED
		case core.WatchEventTypeEnum.Modified():
			pbEv.Type = pb.WatchEvent_MODIFIED
		case core.WatchEventTypeEnum.Deleted():
			pbEv.Type = pb.WatchEvent_DELETED
		case core.WatchEventTypeEnum.Error():
			pbEv.Type = pb.WatchEvent_ERROR
		}
		if err := stream.Send(pbEv); err != nil {
			return err
		}
	}
	return nil
}

// capsuleToProto converts a core CapsuleResource to its proto representation.
func capsuleToProto(c *core.CapsuleResource) *pb.CapsuleResource {
	if c == nil {
		return nil
	}
	r := &pb.CapsuleResource{
		Meta: &pb.ObjectMeta{
			Name:      c.Meta.Name,
			Id:        c.Meta.ID,
			Cluster:   c.Meta.Cluster,
			Labels:    c.Meta.Labels,
			CreatedAt: timestamppb.New(c.Meta.CreatedAt),
			UpdatedAt: timestamppb.New(c.Meta.UpdatedAt),
		},
		Spec: &pb.CapsuleSpec{
			Image:        c.Spec.Image,
			ImageDigest:  c.Spec.ImageDigest,
			Orbit:        c.Spec.Orbit,
			Tier:         c.Spec.Tier,
			CpuCores:     c.Spec.CPUCores,
			MemoryMb:     c.Spec.MemoryMB,
			DiskMb:       c.Spec.DiskMB,
			ReplicasMin:  c.Spec.ReplicasMin,
			ReplicasMax:  c.Spec.ReplicasMax,
			Env:          c.Spec.Env,
			Command:      c.Spec.Command,
			NetworkMode:  c.Spec.NetworkMode,
		},
		Status: &pb.CapsuleStatus{
			Status: c.Status.Status,
		},
	}
	for _, rep := range c.Status.Replicas {
		r.Status.Replicas = append(r.Status.Replicas, &pb.ReplicaStatus{
			ReplicaId: rep.ReplicaID,
			NodeId:    rep.NodeID,
			Status:    rep.Status,
			StartedAt: timestamppb.New(rep.StartedAt),
		})
	}
	return r
}

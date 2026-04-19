package grpc

import (
	"context"

	"github.com/tareksalem/falak/api/core"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

type systemService struct {
	pb.UnimplementedSystemServiceServer
	core *core.Core
}

func (s *systemService) Info(_ context.Context, _ *pb.SystemInfoRequest) (*pb.SystemInfo, error) {
	info := s.core.GetInfo()
	return &pb.SystemInfo{
		NodeId:        info.NodeID,
		NodeName:      info.NodeName,
		Version:       info.Version,
		GoVersion:     info.GoVersion,
		Platform:      info.Platform,
		UptimeSeconds: int64(info.Uptime.Seconds()),
		Clusters:      info.Clusters,
	}, nil
}

func (s *systemService) Version(_ context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	info := s.core.GetInfo()
	return &pb.VersionResponse{
		Version:   info.Version,
		GoVersion: info.GoVersion,
		Platform:  info.Platform,
	}, nil
}

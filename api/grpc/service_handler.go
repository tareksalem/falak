package grpc

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/api/core"
	pb "github.com/tareksalem/falak/api/proto/v1alpha1pb"
)

// serviceService implements pb.ServiceServiceServer by delegating to a
// core.ServiceFacade. Wired in NewServer via core.Core.Services().
type serviceService struct {
	pb.UnimplementedServiceServiceServer
	core *core.Core
}

// facade pulls the ServiceFacade off Core or returns Unavailable.
func (s *serviceService) facade() (core.ServiceFacade, error) {
	f := s.core.Services()
	if f == nil {
		return nil, status.Error(codes.Unavailable, "service facade not configured on this node")
	}
	return f, nil
}

// Create handles ServiceService.Create.
func (s *serviceService) Create(_ context.Context, req *pb.CreateServiceRequest) (*pb.ServiceResource, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	res, err := f.CreateService(core.CreateServiceRequest{
		Cluster: req.Cluster,
		Name:    req.Name,
		Spec:    protoToSpecView(req.Spec),
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return serviceToProto(res), nil
}

// Get handles ServiceService.Get.
func (s *serviceService) Get(_ context.Context, req *pb.GetServiceRequest) (*pb.ServiceResource, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	res, err := f.GetService(core.GetServiceRequest{Cluster: req.Cluster, Name: req.Name})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return serviceToProto(res), nil
}

// List handles ServiceService.List.
func (s *serviceService) List(_ context.Context, req *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	var pag core.Pagination
	if req.Pagination != nil {
		pag.PageSize = req.Pagination.PageSize
		pag.NextPageToken = req.Pagination.NextPageToken
	}
	res, err := f.ListServices(core.ListServicesRequest{
		Cluster:    req.Cluster,
		Visibility: req.Visibility,
		Group:      req.Group,
		Pagination: pag,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	resp := &pb.ListServicesResponse{Paging: &pb.PagedResult{
		NextPageToken: res.Paging.NextPageToken,
		TotalCount:    res.Paging.TotalCount,
	}}
	for i := range res.Services {
		resp.Services = append(resp.Services, serviceToProto(&res.Services[i]))
	}
	return resp, nil
}

// Update handles ServiceService.Update.
func (s *serviceService) Update(_ context.Context, req *pb.UpdateServiceRequest) (*pb.ServiceResource, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	res, err := f.UpdateService(core.UpdateServiceRequest{
		Cluster: req.Cluster,
		Name:    req.Name,
		Spec:    protoToSpecView(req.Spec),
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return serviceToProto(res), nil
}

// Delete handles ServiceService.Delete.
func (s *serviceService) Delete(_ context.Context, req *pb.DeleteServiceRequest) (*emptypb.Empty, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	if err := f.DeleteService(core.DeleteServiceRequest{Cluster: req.Cluster, Name: req.Name}); err != nil {
		return nil, core.ToGRPCError(err)
	}
	return &emptypb.Empty{}, nil
}

// Apply handles ServiceService.Apply (declarative upsert).
func (s *serviceService) Apply(_ context.Context, req *pb.ApplyServiceRequest) (*pb.ServiceResource, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	res, err := f.ApplyService(core.ApplyServiceRequest{
		Cluster: req.Cluster,
		Name:    req.Name,
		Spec:    protoToSpecView(req.Spec),
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return serviceToProto(res), nil
}

// RebindBackend handles ServiceService.RebindBackend.
func (s *serviceService) RebindBackend(_ context.Context, req *pb.RebindServiceBackendRequest) (*pb.ServiceResource, error) {
	f, err := s.facade()
	if err != nil {
		return nil, err
	}
	res, err := f.RebindBackend(core.RebindServiceBackendRequest{
		Cluster: req.Cluster,
		Name:    req.Name,
		Backend: req.Backend,
	})
	if err != nil {
		return nil, core.ToGRPCError(err)
	}
	return serviceToProto(res), nil
}

// Watch streams Service events via the shared watch bus on Core.
func (s *serviceService) Watch(_ *pb.WatchServicesRequest, stream pb.ServiceService_WatchServer) error {
	ch, err := s.core.Watch(stream.Context(), "service")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for ev := range ch {
		out := &pb.WatchEvent{Timestamp: timestamppb.New(ev.Timestamp)}
		switch ev.Type {
		case core.WatchEventTypeEnum.Added():
			out.Type = pb.WatchEvent_ADDED
		case core.WatchEventTypeEnum.Modified():
			out.Type = pb.WatchEvent_MODIFIED
		case core.WatchEventTypeEnum.Deleted():
			out.Type = pb.WatchEvent_DELETED
		case core.WatchEventTypeEnum.Error():
			out.Type = pb.WatchEvent_ERROR
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return nil
}

// --- proto ↔ core view marshalling --------------------------------------

func protoToSpecView(in *pb.ServiceSpec) core.ServiceSpecView {
	if in == nil {
		return core.ServiceSpecView{}
	}
	out := core.ServiceSpecView{
		Visibility: in.Visibility,
		Group:      in.Group,
	}
	for _, p := range in.Ports {
		out.Ports = append(out.Ports, core.ServicePortView{
			Name:     p.Name,
			Port:     uint16(p.Port),
			Protocol: p.Protocol,
		})
	}
	for _, b := range in.Backends {
		out.Backends = append(out.Backends, core.ServiceBackendView{
			Capsule:           b.Capsule,
			CapturedCapsuleID: b.CapturedCapsuleId,
			PortMap:           copyStringMap(b.PortMap),
			Weight:            b.Weight,
		})
	}
	if in.Strategy != nil {
		out.Strategy = &core.StrategyView{Type: in.Strategy.Type}
		if in.Strategy.Canary != nil {
			out.Strategy.Canary = &core.CanaryStrategyView{
				Target:          in.Strategy.Canary.Target,
				From:            in.Strategy.Canary.From,
				Step:            in.Strategy.Canary.Step,
				Interval:        time.Duration(in.Strategy.Canary.IntervalMs) * time.Millisecond,
				SuccessCriteria: append([]string(nil), in.Strategy.Canary.SuccessCriteria...),
				AbortOn:         append([]string(nil), in.Strategy.Canary.AbortOn...),
			}
		}
		if in.Strategy.BlueGreen != nil {
			out.Strategy.BlueGreen = &core.BlueGreenStrategyView{
				Active: in.Strategy.BlueGreen.Active,
				Drain:  time.Duration(in.Strategy.BlueGreen.DrainMs) * time.Millisecond,
			}
		}
	}
	if in.Timeouts != nil {
		out.Timeouts = core.ServiceTimeoutsView{
			Idle:    time.Duration(in.Timeouts.IdleMs) * time.Millisecond,
			Connect: time.Duration(in.Timeouts.ConnectMs) * time.Millisecond,
		}
	}
	return out
}

func serviceToProto(r *core.ServiceResource) *pb.ServiceResource {
	if r == nil {
		return nil
	}
	out := &pb.ServiceResource{
		Meta: &pb.ObjectMeta{
			Name:      r.Meta.Name,
			Id:        r.Meta.ID,
			Cluster:   r.Meta.Cluster,
			Labels:    r.Meta.Labels,
			CreatedAt: timestamppb.New(r.Meta.CreatedAt),
			UpdatedAt: timestamppb.New(r.Meta.UpdatedAt),
		},
		Spec: specViewToProto(r.Spec),
		Status: &pb.ServiceStatus{
			Status:  r.Status.Status,
			Version: r.Status.Version,
		},
	}
	for _, st := range r.Status.BackendStates {
		out.Status.BackendStates = append(out.Status.BackendStates, &pb.BackendState{
			Backend:           st.Backend,
			Resolution:        st.Resolution,
			CapturedCapsuleId: st.CapturedCapsuleID,
			LastResolvedAt:    timestamppb.New(st.LastResolvedAt),
		})
	}
	return out
}

func specViewToProto(s core.ServiceSpecView) *pb.ServiceSpec {
	out := &pb.ServiceSpec{
		Visibility: s.Visibility,
		Group:      s.Group,
		Timeouts: &pb.ServiceTimeouts{
			IdleMs:    s.Timeouts.Idle.Milliseconds(),
			ConnectMs: s.Timeouts.Connect.Milliseconds(),
		},
	}
	for _, p := range s.Ports {
		out.Ports = append(out.Ports, &pb.ServicePort{
			Name:     p.Name,
			Port:     uint32(p.Port),
			Protocol: p.Protocol,
		})
	}
	for _, b := range s.Backends {
		out.Backends = append(out.Backends, &pb.ServiceBackend{
			Capsule:           b.Capsule,
			CapturedCapsuleId: b.CapturedCapsuleID,
			PortMap:           copyStringMap(b.PortMap),
			Weight:            b.Weight,
		})
	}
	if s.Strategy != nil {
		out.Strategy = &pb.Strategy{Type: s.Strategy.Type}
		if s.Strategy.Canary != nil {
			out.Strategy.Canary = &pb.CanaryStrategy{
				Target:          s.Strategy.Canary.Target,
				From:            s.Strategy.Canary.From,
				Step:            s.Strategy.Canary.Step,
				IntervalMs:      s.Strategy.Canary.Interval.Milliseconds(),
				SuccessCriteria: append([]string(nil), s.Strategy.Canary.SuccessCriteria...),
				AbortOn:         append([]string(nil), s.Strategy.Canary.AbortOn...),
			}
		}
		if s.Strategy.BlueGreen != nil {
			out.Strategy.BlueGreen = &pb.BlueGreenStrategy{
				Active:  s.Strategy.BlueGreen.Active,
				DrainMs: s.Strategy.BlueGreen.Drain.Milliseconds(),
			}
		}
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

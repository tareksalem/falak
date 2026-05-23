package service

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// serviceToProto converts a Go *Service into its protobuf representation
// suitable for inclusion in a ServiceUpdate envelope. Nil input returns nil.
//
// Time-based fields are converted via timestamppb and durations are
// serialised as integer seconds (matching the proto schema).
func serviceToProto(s *Service) *servicepb.Service {
	if s == nil {
		return nil
	}
	return &servicepb.Service{
		Id:            s.ID.String(),
		ClusterId:     s.ClusterID,
		Spec:          serviceSpecToProto(&s.Spec),
		Status:        string(s.Status),
		Version:       s.Version,
		CreatedAt:     timestamppb.New(s.CreatedAt),
		UpdatedAt:     timestamppb.New(s.UpdatedAt),
		BackendStates: backendStatesToProto(s.BackendStates),
	}
}

// serviceFromProto reconstructs a Go *Service from its protobuf form.
// Nil input returns nil. Unknown enum strings are passed through verbatim
// — callers may run ValidateSpec on the result if strict admission is
// required.
func serviceFromProto(pb *servicepb.Service) *Service {
	if pb == nil {
		return nil
	}
	s := &Service{
		ID:            ServiceID(pb.Id),
		ClusterID:     pb.ClusterId,
		Status:        ServiceStatus(pb.Status),
		Version:       pb.Version,
		BackendStates: backendStatesFromProto(pb.BackendStates),
	}
	if pb.Spec != nil {
		s.Spec = serviceSpecFromProto(pb.Spec)
	}
	if pb.CreatedAt != nil {
		s.CreatedAt = pb.CreatedAt.AsTime()
	}
	if pb.UpdatedAt != nil {
		s.UpdatedAt = pb.UpdatedAt.AsTime()
	}
	return s
}

func backendStatesToProto(in []BackendState) []*servicepb.BackendState {
	if len(in) == 0 {
		return nil
	}
	out := make([]*servicepb.BackendState, 0, len(in))
	for _, b := range in {
		out = append(out, &servicepb.BackendState{
			Name:              b.Name,
			Resolution:        string(b.Resolution),
			CapturedCapsuleId: b.CapturedCapsuleID,
			LastResolvedAt:    timestamppb.New(b.LastResolvedAt),
		})
	}
	return out
}

func backendStatesFromProto(in []*servicepb.BackendState) []BackendState {
	if len(in) == 0 {
		return nil
	}
	out := make([]BackendState, 0, len(in))
	for _, b := range in {
		bs := BackendState{
			Name:              b.Name,
			Resolution:        BackendResolution(b.Resolution),
			CapturedCapsuleID: b.CapturedCapsuleId,
		}
		if b.LastResolvedAt != nil {
			bs.LastResolvedAt = b.LastResolvedAt.AsTime()
		}
		out = append(out, bs)
	}
	return out
}

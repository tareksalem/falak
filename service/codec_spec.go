package service

import (
	"time"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// serviceSpecToProto converts a Go *ServiceSpec into its protobuf form.
func serviceSpecToProto(s *ServiceSpec) *servicepb.ServiceSpec {
	if s == nil {
		return nil
	}
	pb := &servicepb.ServiceSpec{
		Name:       s.Name,
		Visibility: string(s.Visibility),
		Group:      s.Group,
		Timeouts: &servicepb.ServiceTimeouts{
			IdleSeconds:    int32(s.Timeouts.Idle / time.Second),
			ConnectSeconds: int32(s.Timeouts.Connect / time.Second),
		},
	}
	for _, p := range s.Ports {
		pb.Ports = append(pb.Ports, &servicepb.ServicePort{
			Name:     p.Name,
			Port:     uint32(p.Port),
			Protocol: string(p.Protocol),
		})
	}
	for _, b := range s.Backends {
		bp := &servicepb.ServiceBackend{
			Capsule:           b.Capsule,
			CapturedCapsuleId: b.CapturedCapsuleID,
			Weight:            b.Weight,
		}
		if len(b.PortMap) > 0 {
			bp.PortMap = make(map[string]string, len(b.PortMap))
			for k, v := range b.PortMap {
				bp.PortMap[k] = v
			}
		}
		pb.Backends = append(pb.Backends, bp)
	}
	if s.Strategy != nil {
		pb.Strategy = strategyToProto(s.Strategy)
	}
	return pb
}

// serviceSpecFromProto reconstructs a ServiceSpec from its protobuf form.
func serviceSpecFromProto(pb *servicepb.ServiceSpec) ServiceSpec {
	s := ServiceSpec{
		Name:       pb.Name,
		Visibility: Visibility(pb.Visibility),
		Group:      pb.Group,
	}
	for _, p := range pb.Ports {
		s.Ports = append(s.Ports, ServicePort{
			Name:     p.Name,
			Port:     uint16(p.Port),
			Protocol: Protocol(p.Protocol),
		})
	}
	for _, b := range pb.Backends {
		sb := ServiceBackend{
			Capsule:           b.Capsule,
			CapturedCapsuleID: b.CapturedCapsuleId,
			Weight:            b.Weight,
		}
		if len(b.PortMap) > 0 {
			sb.PortMap = make(map[string]string, len(b.PortMap))
			for k, v := range b.PortMap {
				sb.PortMap[k] = v
			}
		}
		s.Backends = append(s.Backends, sb)
	}
	if pb.Strategy != nil {
		s.Strategy = strategyFromProto(pb.Strategy)
	}
	if pb.Timeouts != nil {
		s.Timeouts.Idle = time.Duration(pb.Timeouts.IdleSeconds) * time.Second
		s.Timeouts.Connect = time.Duration(pb.Timeouts.ConnectSeconds) * time.Second
	}
	return s
}

func strategyToProto(s *Strategy) *servicepb.Strategy {
	pb := &servicepb.Strategy{Type: string(s.Type)}
	if s.Canary != nil {
		pb.Canary = &servicepb.CanaryStrategy{
			Target:          s.Canary.Target,
			From:            s.Canary.From,
			Step:            s.Canary.Step,
			IntervalSeconds: int32(s.Canary.Interval / time.Second),
			SuccessCriteria: append([]string(nil), s.Canary.SuccessCriteria...),
			AbortOn:         append([]string(nil), s.Canary.AbortOn...),
		}
	}
	if s.BlueGreen != nil {
		pb.BlueGreen = &servicepb.BlueGreenStrategy{
			Active:       s.BlueGreen.Active,
			DrainSeconds: int32(s.BlueGreen.Drain / time.Second),
		}
	}
	return pb
}

func strategyFromProto(pb *servicepb.Strategy) *Strategy {
	s := &Strategy{Type: StrategyType(pb.Type)}
	if pb.Canary != nil {
		s.Canary = &CanaryStrategy{
			Target:          pb.Canary.Target,
			From:            pb.Canary.From,
			Step:            pb.Canary.Step,
			Interval:        time.Duration(pb.Canary.IntervalSeconds) * time.Second,
			SuccessCriteria: append([]string(nil), pb.Canary.SuccessCriteria...),
			AbortOn:         append([]string(nil), pb.Canary.AbortOn...),
		}
	}
	if pb.BlueGreen != nil {
		s.BlueGreen = &BlueGreenStrategy{
			Active: pb.BlueGreen.Active,
			Drain:  time.Duration(pb.BlueGreen.DrainSeconds) * time.Second,
		}
	}
	return s
}

package node

import (
	"context"
	"fmt"

	"github.com/tareksalem/falak/api/core"
	"github.com/tareksalem/falak/service"
)

// APIServiceFacade adapts the node's ServiceHandler to core.ServiceFacade.
// It is intentionally separate from APIFacade so a node started with the
// service mesh disabled still satisfies NodeFacade — the ServiceFacade is
// optional and wired with core.WithServices.
type APIServiceFacade struct {
	node *Node
}

// NewAPIServiceFacade returns the adapter. The caller is responsible for
// only wiring it when ServiceHandler() is non-nil and its Manager() is
// active — see runDaemon.
func NewAPIServiceFacade(n *Node) *APIServiceFacade { return &APIServiceFacade{node: n} }

func (s *APIServiceFacade) manager() (*service.Manager, error) {
	h := s.node.ServiceHandler()
	if h == nil {
		return nil, fmt.Errorf("%w: service mesh disabled on this node", core.ErrUnavailable)
	}
	m := h.Manager()
	if m == nil {
		return nil, fmt.Errorf("%w: service manager not started", core.ErrUnavailable)
	}
	return m, nil
}

func (s *APIServiceFacade) clusterID(provided string) string {
	if provided != "" {
		return provided
	}
	clusters := s.node.JoinedClusters()
	if len(clusters) > 0 {
		return clusters[0]
	}
	return ""
}

// CreateService validates the request and forwards it to service.Manager.
func (s *APIServiceFacade) CreateService(req core.CreateServiceRequest) (*core.ServiceResource, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	cluster := s.clusterID(req.Cluster)
	if cluster == "" {
		return nil, core.WrapInvalidArgument("cluster is required")
	}
	spec := apiSpecToServiceSpec(req.Name, req.Spec)
	svc, err := mgr.Create(context.Background(), cluster, spec)
	if err != nil {
		return nil, err
	}
	r := serviceToResource(svc)
	return &r, nil
}

// GetService returns a Service by name.
func (s *APIServiceFacade) GetService(req core.GetServiceRequest) (*core.ServiceResource, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	svc := mgr.GetByName(req.Name)
	if svc == nil {
		return nil, core.WrapNotFound(fmt.Sprintf("service %q", req.Name))
	}
	r := serviceToResource(svc)
	return &r, nil
}

// ListServices returns all Services this node knows about.
func (s *APIServiceFacade) ListServices(req core.ListServicesRequest) (*core.ListServicesResponse, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	var all []*service.Service
	switch {
	case req.Visibility != "":
		all = mgr.ListByVisibility(service.Visibility(req.Visibility))
	case req.Group != "":
		all = mgr.ListByGroup(req.Group)
	default:
		all = mgr.List()
	}
	out := &core.ListServicesResponse{
		Services: make([]core.ServiceResource, 0, len(all)),
	}
	for _, svc := range all {
		out.Services = append(out.Services, serviceToResource(svc))
	}
	return out, nil
}

// UpdateService replaces a Service spec in place.
func (s *APIServiceFacade) UpdateService(req core.UpdateServiceRequest) (*core.ServiceResource, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	existing := mgr.GetByName(req.Name)
	if existing == nil {
		return nil, core.WrapNotFound(fmt.Sprintf("service %q", req.Name))
	}
	spec := apiSpecToServiceSpec(req.Name, req.Spec)
	svc, err := mgr.Update(context.Background(), existing.ID, spec)
	if err != nil {
		return nil, err
	}
	r := serviceToResource(svc)
	return &r, nil
}

// DeleteService removes a Service. Capsules are untouched.
func (s *APIServiceFacade) DeleteService(req core.DeleteServiceRequest) error {
	mgr, err := s.manager()
	if err != nil {
		return err
	}
	existing := mgr.GetByName(req.Name)
	if existing == nil {
		return core.WrapNotFound(fmt.Sprintf("service %q", req.Name))
	}
	return mgr.Delete(context.Background(), existing.ID)
}

// ApplyService is the declarative upsert used by `falak service apply`.
func (s *APIServiceFacade) ApplyService(req core.ApplyServiceRequest) (*core.ServiceResource, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	cluster := s.clusterID(req.Cluster)
	if cluster == "" {
		return nil, core.WrapInvalidArgument("cluster is required")
	}
	spec := apiSpecToServiceSpec(req.Name, req.Spec)
	svc, err := mgr.Apply(context.Background(), cluster, spec)
	if err != nil {
		return nil, err
	}
	r := serviceToResource(svc)
	return &r, nil
}

// RebindBackend clears the captured-identity binding on a backend.
func (s *APIServiceFacade) RebindBackend(req core.RebindServiceBackendRequest) (*core.ServiceResource, error) {
	mgr, err := s.manager()
	if err != nil {
		return nil, err
	}
	existing := mgr.GetByName(req.Name)
	if existing == nil {
		return nil, core.WrapNotFound(fmt.Sprintf("service %q", req.Name))
	}
	// The Manager doesn't expose a single-backend Rebind; the operator
	// path drives it by updating the spec which clears the captured ID
	// on touched backends. Round-trip the current spec so the existing
	// Update path triggers re-resolution.
	svc, err := mgr.Update(context.Background(), existing.ID, existing.Spec)
	if err != nil {
		return nil, err
	}
	r := serviceToResource(svc)
	return &r, nil
}

// --- translation helpers -------------------------------------------------

func apiSpecToServiceSpec(name string, view core.ServiceSpecView) service.ServiceSpec {
	spec := service.ServiceSpec{
		Name:       name,
		Visibility: service.Visibility(view.Visibility),
		Group:      view.Group,
		Timeouts: service.ServiceTimeouts{
			Idle:    view.Timeouts.Idle,
			Connect: view.Timeouts.Connect,
		},
	}
	for _, p := range view.Ports {
		spec.Ports = append(spec.Ports, service.ServicePort{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: service.Protocol(p.Protocol),
		})
	}
	for _, b := range view.Backends {
		spec.Backends = append(spec.Backends, service.ServiceBackend{
			Capsule:           b.Capsule,
			CapturedCapsuleID: b.CapturedCapsuleID,
			PortMap:           b.PortMap,
			Weight:            b.Weight,
		})
	}
	if view.Strategy != nil {
		spec.Strategy = &service.Strategy{Type: service.StrategyType(view.Strategy.Type)}
		if view.Strategy.Canary != nil {
			spec.Strategy.Canary = &service.CanaryStrategy{
				Target:          view.Strategy.Canary.Target,
				From:            view.Strategy.Canary.From,
				Step:            view.Strategy.Canary.Step,
				Interval:        view.Strategy.Canary.Interval,
				SuccessCriteria: view.Strategy.Canary.SuccessCriteria,
				AbortOn:         view.Strategy.Canary.AbortOn,
			}
		}
		if view.Strategy.BlueGreen != nil {
			spec.Strategy.BlueGreen = &service.BlueGreenStrategy{
				Active: view.Strategy.BlueGreen.Active,
				Drain:  view.Strategy.BlueGreen.Drain,
			}
		}
	}
	return spec
}

func serviceToResource(svc *service.Service) core.ServiceResource {
	ports := make([]core.ServicePortView, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, core.ServicePortView{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: string(p.Protocol),
		})
	}
	backends := make([]core.ServiceBackendView, 0, len(svc.Spec.Backends))
	for _, b := range svc.Spec.Backends {
		backends = append(backends, core.ServiceBackendView{
			Capsule:           b.Capsule,
			CapturedCapsuleID: b.CapturedCapsuleID,
			PortMap:           b.PortMap,
			Weight:            b.Weight,
		})
	}
	specView := core.ServiceSpecView{
		Visibility: string(svc.Spec.Visibility),
		Group:      svc.Spec.Group,
		Ports:      ports,
		Backends:   backends,
		Timeouts: core.ServiceTimeoutsView{
			Idle:    svc.Spec.Timeouts.Idle,
			Connect: svc.Spec.Timeouts.Connect,
		},
	}
	if svc.Spec.Strategy != nil {
		specView.Strategy = &core.StrategyView{Type: string(svc.Spec.Strategy.Type)}
		if svc.Spec.Strategy.Canary != nil {
			specView.Strategy.Canary = &core.CanaryStrategyView{
				Target:          svc.Spec.Strategy.Canary.Target,
				From:            svc.Spec.Strategy.Canary.From,
				Step:            svc.Spec.Strategy.Canary.Step,
				Interval:        svc.Spec.Strategy.Canary.Interval,
				SuccessCriteria: svc.Spec.Strategy.Canary.SuccessCriteria,
				AbortOn:         svc.Spec.Strategy.Canary.AbortOn,
			}
		}
		if svc.Spec.Strategy.BlueGreen != nil {
			specView.Strategy.BlueGreen = &core.BlueGreenStrategyView{
				Active: svc.Spec.Strategy.BlueGreen.Active,
				Drain:  svc.Spec.Strategy.BlueGreen.Drain,
			}
		}
	}

	states := make([]core.BackendStateView, 0, len(svc.BackendStates))
	for _, st := range svc.BackendStates {
		states = append(states, core.BackendStateView{
			Backend:           st.Name,
			Resolution:        string(st.Resolution),
			CapturedCapsuleID: st.CapturedCapsuleID,
			LastResolvedAt:    st.LastResolvedAt,
		})
	}
	return core.ServiceResource{
		Meta: core.ObjectMeta{
			ID:        string(svc.ID),
			Name:      svc.Spec.Name,
			Cluster:   svc.ClusterID,
			CreatedAt: svc.CreatedAt,
			UpdatedAt: svc.UpdatedAt,
		},
		Spec: specView,
		Status: core.ServiceStatusView{
			Status:        string(svc.Status),
			Version:       svc.Version,
			BackendStates: states,
		},
	}
}

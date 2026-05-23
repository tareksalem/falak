package core

import "time"

// ServiceFacade is the narrow surface the gRPC ServiceService server
// depends on. The real Node ServiceHandler implements it via an adapter
// that delegates to service.Manager; tests can use a fake.
//
// Mirrors the pattern of NodeFacade.CapsuleCreate/...: keep the
// transport layer ignorant of internal types, route every call through
// a typed request/response struct so encoding stays at the boundary.
type ServiceFacade interface {
	// CreateService admits a new Service.
	CreateService(req CreateServiceRequest) (*ServiceResource, error)

	// GetService returns a Service by name (or by ID when Name is empty).
	GetService(req GetServiceRequest) (*ServiceResource, error)

	// ListServices returns the Service set known to this node.
	ListServices(req ListServicesRequest) (*ListServicesResponse, error)

	// UpdateService replaces an existing Service's spec.
	UpdateService(req UpdateServiceRequest) (*ServiceResource, error)

	// DeleteService removes a Service. Capsules are never touched.
	DeleteService(req DeleteServiceRequest) error

	// ApplyService is the declarative upsert path used by `falak service apply`.
	ApplyService(req ApplyServiceRequest) (*ServiceResource, error)

	// RebindBackend clears the captured capsule ID on the named backend
	// so the next resolve pass binds the current capsule with that name.
	RebindBackend(req RebindServiceBackendRequest) (*ServiceResource, error)
}

// ServiceResource is the API view of a Service (meta + spec + status).
type ServiceResource struct {
	Meta   ObjectMeta
	Spec   ServiceSpecView
	Status ServiceStatusView
}

// ServiceSpecView mirrors service.ServiceSpec at the API boundary.
type ServiceSpecView struct {
	Visibility string
	Group      string
	Ports      []ServicePortView
	Backends   []ServiceBackendView
	Strategy   *StrategyView
	Timeouts   ServiceTimeoutsView
}

// ServicePortView mirrors service.ServicePort.
type ServicePortView struct {
	Name     string
	Port     uint16
	Protocol string
}

// ServiceBackendView mirrors service.ServiceBackend.
type ServiceBackendView struct {
	Capsule           string
	CapturedCapsuleID string
	PortMap           map[string]string
	Weight            int32
}

// StrategyView mirrors service.Strategy.
type StrategyView struct {
	Type      string
	Canary    *CanaryStrategyView
	BlueGreen *BlueGreenStrategyView
}

// CanaryStrategyView mirrors service.CanaryStrategy. Interval is the
// canonical strategy tick wait.
type CanaryStrategyView struct {
	Target          string
	From            string
	Step            int32
	Interval        time.Duration
	SuccessCriteria []string
	AbortOn         []string
}

// BlueGreenStrategyView mirrors service.BlueGreenStrategy.
type BlueGreenStrategyView struct {
	Active string
	Drain  time.Duration
}

// ServiceTimeoutsView mirrors service.ServiceTimeouts.
type ServiceTimeoutsView struct {
	Idle    time.Duration
	Connect time.Duration
}

// ServiceStatusView mirrors a Service's lifecycle status + backend states.
type ServiceStatusView struct {
	Status        string
	Version       string
	BackendStates []BackendStateView
}

// BackendStateView mirrors service.BackendState.
type BackendStateView struct {
	Backend           string
	Resolution        string
	CapturedCapsuleID string
	LastResolvedAt    time.Time
}

// --- request types -------------------------------------------------------

// CreateServiceRequest is the input for ServiceFacade.CreateService.
type CreateServiceRequest struct {
	Cluster string
	Name    string
	Spec    ServiceSpecView
}

// GetServiceRequest is the input for ServiceFacade.GetService.
type GetServiceRequest struct {
	Cluster string
	Name    string
}

// ListServicesRequest is the input for ServiceFacade.ListServices.
type ListServicesRequest struct {
	Cluster    string
	Visibility string
	Group      string
	Pagination Pagination
}

// ListServicesResponse is the output of ServiceFacade.ListServices.
type ListServicesResponse struct {
	Services []ServiceResource
	Paging   PagedResult
}

// UpdateServiceRequest is the input for ServiceFacade.UpdateService.
type UpdateServiceRequest struct {
	Cluster string
	Name    string
	Spec    ServiceSpecView
}

// DeleteServiceRequest is the input for ServiceFacade.DeleteService.
type DeleteServiceRequest struct {
	Cluster string
	Name    string
}

// ApplyServiceRequest is the input for ServiceFacade.ApplyService.
type ApplyServiceRequest struct {
	Cluster string
	Name    string
	Spec    ServiceSpecView
}

// RebindServiceBackendRequest is the input for ServiceFacade.RebindBackend.
type RebindServiceBackendRequest struct {
	Cluster string
	Name    string
	Backend string
}

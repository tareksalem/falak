package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// stubNode implements NodeFacade for testing.
type stubNode struct {
	capsules map[string]*CapsuleResource
	ready    bool
}

func newStubNode() *stubNode {
	return &stubNode{
		capsules: make(map[string]*CapsuleResource),
		ready:    true,
	}
}

func (s *stubNode) NodeID() string            { return "test-node-id" }
func (s *stubNode) NodeName() string           { return "test-node" }
func (s *stubNode) ListenAddrs() []string      { return []string{"/ip4/127.0.0.1/tcp/4001"} }
func (s *stubNode) IsReady() bool              { return s.ready }
func (s *stubNode) StartedAt() time.Time       { return time.Time{} }
func (s *stubNode) ClusterJoin(ctx context.Context, req JoinClusterRequest) error { return nil }
func (s *stubNode) ClusterLeave(ctx context.Context, path string) error           { return nil }
func (s *stubNode) ClusterList(ctx context.Context) (*ListClustersResponse, error) {
	return &ListClustersResponse{}, nil
}
func (s *stubNode) NodeList(ctx context.Context, req ListNodesRequest) (*ListNodesResponse, error) {
	return &ListNodesResponse{}, nil
}
func (s *stubNode) NodeGet(ctx context.Context, cluster, nodeID string) (*NodeResource, error) {
	return nil, WrapNotFound("node " + nodeID)
}
func (s *stubNode) WatchEvents(ctx context.Context, resourceType string) (<-chan WatchEvent, error) {
	ch := make(chan WatchEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func (s *stubNode) CapsuleCreate(_ context.Context, cluster string, req CreateCapsuleRequest) (*CapsuleResource, error) {
	id := fmt.Sprintf("cap-%d", len(s.capsules)+1)
	r := &CapsuleResource{
		Meta: ObjectMeta{
			Name:      req.Name,
			ID:        id,
			Cluster:   cluster,
			Labels:    req.Labels,
			CreatedAt: time.Now(),
		},
		Spec: CapsuleSpecView{
			Image:       req.Image,
			Orbit:       req.Orbit,
			Tier:        req.Tier,
			CPUCores:    req.CPUCores,
			MemoryMB:    req.MemoryMB,
			NetworkMode: req.NetworkMode,
		},
		Status: CapsuleStatusView{Status: "announced"},
	}
	s.capsules[id] = r
	return r, nil
}

func (s *stubNode) CapsuleGet(_ context.Context, id string) (*CapsuleResource, error) {
	r, ok := s.capsules[id]
	if !ok {
		return nil, nil
	}
	return r, nil
}

func (s *stubNode) CapsuleList(_ context.Context, req ListCapsulesRequest) (*ListCapsulesResponse, error) {
	var caps []CapsuleResource
	for _, c := range s.capsules {
		caps = append(caps, *c)
	}
	return &ListCapsulesResponse{Capsules: caps}, nil
}

func (s *stubNode) CapsuleDelete(_ context.Context, id string) error {
	if _, ok := s.capsules[id]; !ok {
		return WrapNotFound("capsule " + id)
	}
	delete(s.capsules, id)
	return nil
}

func (s *stubNode) CapsuleUpdate(_ context.Context, req UpdateCapsuleRequest) (*CapsuleResource, error) {
	r, ok := s.capsules[req.ID]
	if !ok {
		return nil, WrapNotFound("capsule " + req.ID)
	}
	if req.Image != "" {
		r.Spec.Image = req.Image
	}
	return r, nil
}

// --- Tests ---------------------------------------------------------------

func TestHealthz(t *testing.T) {
	c := New(newStubNode())
	if err := c.Healthz(); err != nil {
		t.Fatalf("Healthz: %v", err)
	}
}

func TestReadyz_Ready(t *testing.T) {
	c := New(newStubNode())
	if err := c.Readyz(); err != nil {
		t.Fatalf("Readyz: %v", err)
	}
}

func TestReadyz_NotReady(t *testing.T) {
	n := newStubNode()
	n.ready = false
	c := New(n)
	if err := c.Readyz(); err == nil {
		t.Error("expected error when not ready")
	}
}

func TestCreateCapsule(t *testing.T) {
	c := New(newStubNode())
	ctx := context.Background()

	r, err := c.CreateCapsule(ctx, CreateCapsuleRequest{
		Cluster: "test/dc1",
		Name:    "my-app",
		Image:   "img:v1",
		Orbit:   "api",
	})
	if err != nil {
		t.Fatalf("CreateCapsule: %v", err)
	}
	if r.Meta.Name != "my-app" {
		t.Errorf("name = %q, want my-app", r.Meta.Name)
	}
	if r.Spec.Image != "img:v1" {
		t.Errorf("image = %q, want img:v1", r.Spec.Image)
	}
}

func TestCreateCapsule_Validation(t *testing.T) {
	c := New(newStubNode())
	ctx := context.Background()

	tests := []struct {
		name string
		req  CreateCapsuleRequest
	}{
		{"missing cluster", CreateCapsuleRequest{Name: "a", Image: "i", Orbit: "o"}},
		{"missing name", CreateCapsuleRequest{Cluster: "c", Image: "i", Orbit: "o"}},
		{"missing image", CreateCapsuleRequest{Cluster: "c", Name: "a", Orbit: "o"}},
		{"missing orbit", CreateCapsuleRequest{Cluster: "c", Name: "a", Image: "i"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateCapsule(ctx, tt.req)
			if err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestGetCapsule(t *testing.T) {
	c := New(newStubNode())
	ctx := context.Background()

	created, _ := c.CreateCapsule(ctx, CreateCapsuleRequest{
		Cluster: "c", Name: "app", Image: "i", Orbit: "o",
	})

	got, err := c.GetCapsule(ctx, GetCapsuleRequest{ID: created.Meta.ID})
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if got.Meta.ID != created.Meta.ID {
		t.Error("IDs don't match")
	}
}

func TestGetCapsule_NotFound(t *testing.T) {
	c := New(newStubNode())
	_, err := c.GetCapsule(context.Background(), GetCapsuleRequest{ID: "nope"})
	if err == nil {
		t.Error("expected not found error")
	}
}

func TestDeleteCapsule(t *testing.T) {
	c := New(newStubNode())
	ctx := context.Background()

	created, _ := c.CreateCapsule(ctx, CreateCapsuleRequest{
		Cluster: "c", Name: "app", Image: "i", Orbit: "o",
	})

	if err := c.DeleteCapsule(ctx, DeleteCapsuleRequest{ID: created.Meta.ID}); err != nil {
		t.Fatalf("DeleteCapsule: %v", err)
	}

	_, err := c.GetCapsule(ctx, GetCapsuleRequest{ID: created.Meta.ID})
	if err == nil {
		t.Error("expected not found after delete")
	}
}

func TestListCapsules(t *testing.T) {
	c := New(newStubNode())
	ctx := context.Background()

	c.CreateCapsule(ctx, CreateCapsuleRequest{Cluster: "c", Name: "a1", Image: "i", Orbit: "o"})
	c.CreateCapsule(ctx, CreateCapsuleRequest{Cluster: "c", Name: "a2", Image: "i", Orbit: "o"})

	resp, err := c.ListCapsules(ctx, ListCapsulesRequest{})
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(resp.Capsules) != 2 {
		t.Errorf("expected 2 capsules, got %d", len(resp.Capsules))
	}
}

func TestGetInfo(t *testing.T) {
	c := New(newStubNode())
	info := c.GetInfo()
	if info.NodeName != "test-node" {
		t.Errorf("name = %q, want test-node", info.NodeName)
	}
	if info.Version == "" {
		t.Error("version should not be empty")
	}
}

func TestToGRPCError(t *testing.T) {
	err := WrapNotFound("capsule abc")
	grpcErr := ToGRPCError(err)
	if grpcErr == nil {
		t.Fatal("expected gRPC error")
	}
	// The error message should contain the original reason.
	if got := grpcErr.Error(); got == "" {
		t.Error("gRPC error message should not be empty")
	}
}

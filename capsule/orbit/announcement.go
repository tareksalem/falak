package orbit

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/capsule"
	capsulePb "github.com/tareksalem/falak/capsule/proto/capsulepb"
)

// Signer signs orbit message content with the local node's private key.
// The content is the canonical byte sequence of (type + payload + senderID + timestamp).
type Signer interface {
	Sign(content []byte) ([]byte, error)
}

// Verifier verifies that a signature over the canonical content matches
// the public key associated with senderID.
type Verifier interface {
	Verify(senderID string, content []byte, signature []byte) bool
}

// Announcer publishes capsule announcements, status updates, and withdrawals to orbit topics.
type Announcer struct {
	manager  *Manager
	dedup    *Deduplicator
	nodeID   string
	signer   Signer
	verifier Verifier
	logger   *zap.Logger
}

// AnnouncerOption configures an Announcer.
type AnnouncerOption func(*Announcer)

// WithAnnouncerLogger sets the logger.
func WithAnnouncerLogger(logger *zap.Logger) AnnouncerOption {
	return func(a *Announcer) {
		a.logger = logger
	}
}

// WithAnnouncerNodeID sets the local node ID.
func WithAnnouncerNodeID(id string) AnnouncerOption {
	return func(a *Announcer) {
		a.nodeID = id
	}
}

// WithAnnouncerDedup sets the deduplicator.
func WithAnnouncerDedup(d *Deduplicator) AnnouncerOption {
	return func(a *Announcer) {
		a.dedup = d
	}
}

// WithAnnouncerSigner sets the signer for outgoing messages.
func WithAnnouncerSigner(s Signer) AnnouncerOption {
	return func(a *Announcer) {
		a.signer = s
	}
}

// WithAnnouncerVerifier sets the verifier for incoming messages.
func WithAnnouncerVerifier(v Verifier) AnnouncerOption {
	return func(a *Announcer) {
		a.verifier = v
	}
}

// NewAnnouncer creates a new capsule announcer.
func NewAnnouncer(manager *Manager, opts ...AnnouncerOption) *Announcer {
	a := &Announcer{
		manager: manager,
		dedup:   NewDeduplicator(),
		logger:  zap.NewNop(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Announce publishes a capsule announcement to its orbit.
func (a *Announcer) Announce(ctx context.Context, c *capsule.Capsule) error {
	announcement := &capsulePb.CapsuleAnnouncement{
		Capsule:        capsuleToProto(c),
		AnnouncingNode: a.nodeID,
	}

	payload, err := proto.Marshal(announcement)
	if err != nil {
		return fmt.Errorf("failed to marshal announcement: %w", err)
	}

	data, err := a.buildSignedMessage("announcement", payload)
	if err != nil {
		return err
	}

	if err := a.manager.Publish(ctx, c.Spec.Orbit, data); err != nil {
		return fmt.Errorf("failed to publish announcement: %w", err)
	}

	a.logger.Info("capsule announced",
		zap.String("capsule_id", c.ID.String()),
		zap.String("name", c.Spec.Name),
		zap.String("cluster", c.ClusterID),
		zap.String("orbit", c.Spec.Orbit))
	return nil
}

// UpdateStatus publishes a capsule status update.
func (a *Announcer) UpdateStatus(ctx context.Context, c *capsule.Capsule) error {
	update := &capsulePb.CapsuleStatusUpdate{
		CapsuleId: c.ID.String(),
		Status:    string(c.Status),
		Replicas:  replicaStatesToProto(c.Replicas),
		Momentum: &capsulePb.MomentumState{
			Current:      c.Momentum.Current,
			Base:         c.Momentum.Base,
			LastAdjusted: timestamppb.New(c.Momentum.LastAdjusted),
		},
		Timestamp: timestamppb.Now(),
	}

	payload, err := proto.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal status update: %w", err)
	}

	data, err := a.buildSignedMessage("status", payload)
	if err != nil {
		return err
	}

	if err := a.manager.Publish(ctx, c.Spec.Orbit, data); err != nil {
		return fmt.Errorf("failed to publish status update: %w", err)
	}

	a.logger.Debug("capsule status updated",
		zap.String("capsule_id", c.ID.String()),
		zap.String("cluster", c.ClusterID),
		zap.String("orbit", c.Spec.Orbit),
		zap.String("status", string(c.Status)))
	return nil
}

// Withdraw publishes a capsule withdrawal (deleted/stopped/replaced).
func (a *Announcer) Withdraw(ctx context.Context, orbitName string, capsuleID capsule.CapsuleID, reason string) error {
	withdrawal := &capsulePb.CapsuleWithdrawal{
		CapsuleId: capsuleID.String(),
		Reason:    reason,
	}

	payload, err := proto.Marshal(withdrawal)
	if err != nil {
		return fmt.Errorf("failed to marshal withdrawal: %w", err)
	}

	data, err := a.buildSignedMessage("withdrawal", payload)
	if err != nil {
		return err
	}

	if err := a.manager.Publish(ctx, orbitName, data); err != nil {
		return fmt.Errorf("failed to publish withdrawal: %w", err)
	}

	a.logger.Info("capsule withdrawn",
		zap.String("capsule_id", capsuleID.String()),
		zap.String("orbit", orbitName),
		zap.String("reason", reason))
	return nil
}

// buildSignedMessage creates an envelope, signs it, and returns the marshaled bytes.
// If no signer is configured, the signature field is left empty (useful for tests).
func (a *Announcer) buildSignedMessage(msgType string, payload []byte) ([]byte, error) {
	msg := &capsulePb.OrbitMessage{
		Type:      msgType,
		Payload:   payload,
		SenderId:  a.nodeID,
		Timestamp: timestamppb.Now(),
	}

	if a.signer != nil {
		content := canonicalContent(msg)
		sig, err := a.signer.Sign(content)
		if err != nil {
			return nil, fmt.Errorf("failed to sign %s message: %w", msgType, err)
		}
		msg.Signature = sig
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal orbit message: %w", err)
	}
	return data, nil
}

// HandleMessage processes an incoming orbit message.
// Returns the message type and parsed payload, or an error.
func (a *Announcer) HandleMessage(data []byte) (string, proto.Message, error) {
	if a.dedup.IsDuplicate(data) {
		return "", nil, fmt.Errorf("duplicate message")
	}

	var msg capsulePb.OrbitMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return "", nil, fmt.Errorf("failed to unmarshal orbit message: %w", err)
	}

	// Reject stale messages
	if msg.Timestamp != nil {
		age := time.Since(msg.Timestamp.AsTime())
		if age > MaxMessageAge {
			return "", nil, fmt.Errorf("stale message (age=%v)", age)
		}
	}

	// Verify signature if a verifier is configured.
	if a.verifier != nil {
		if len(msg.Signature) == 0 {
			return "", nil, fmt.Errorf("missing signature")
		}
		// Zero the signature field for canonical content reconstruction.
		sig := msg.Signature
		msg.Signature = nil
		content := canonicalContent(&msg)
		msg.Signature = sig

		if !a.verifier.Verify(msg.SenderId, content, sig) {
			return "", nil, fmt.Errorf("invalid signature from sender %q", msg.SenderId)
		}
	}

	switch msg.Type {
	case "announcement":
		var announcement capsulePb.CapsuleAnnouncement
		if err := proto.Unmarshal(msg.Payload, &announcement); err != nil {
			return "", nil, fmt.Errorf("failed to unmarshal announcement: %w", err)
		}
		return msg.Type, &announcement, nil

	case "status":
		var update capsulePb.CapsuleStatusUpdate
		if err := proto.Unmarshal(msg.Payload, &update); err != nil {
			return "", nil, fmt.Errorf("failed to unmarshal status update: %w", err)
		}
		return msg.Type, &update, nil

	case "withdrawal":
		var withdrawal capsulePb.CapsuleWithdrawal
		if err := proto.Unmarshal(msg.Payload, &withdrawal); err != nil {
			return "", nil, fmt.Errorf("failed to unmarshal withdrawal: %w", err)
		}
		return msg.Type, &withdrawal, nil

	default:
		return "", nil, fmt.Errorf("unknown orbit message type %q", msg.Type)
	}
}

// --- Proto conversion helpers ---

func capsuleToProto(c *capsule.Capsule) *capsulePb.Capsule {
	return &capsulePb.Capsule{
		Id:        c.ID.String(),
		ClusterId: c.ClusterID,
		Spec:      specToProto(&c.Spec),
		Status:    string(c.Status),
		Replicas:  replicaStatesToProto(c.Replicas),
		Version:   c.Version,
		CreatedAt: timestamppb.New(c.CreatedAt),
		UpdatedAt: timestamppb.New(c.UpdatedAt),
		Momentum: &capsulePb.MomentumState{
			Current:      c.Momentum.Current,
			Base:         c.Momentum.Base,
			LastAdjusted: timestamppb.New(c.Momentum.LastAdjusted),
		},
	}
}

func specToProto(s *capsule.CapsuleSpec) *capsulePb.CapsuleSpec {
	pb := &capsulePb.CapsuleSpec{
		Name:        s.Name,
		Image:       s.Image,
		ImageDigest: s.ImageDigest,
		Orbit:       s.Orbit,
		Tier:        string(s.Tier),
		Labels:      s.Labels,
		Resources: &capsulePb.ResourceRequirements{
			CpuCores: s.Resources.CPUCores,
			MemoryMb: s.Resources.MemoryMB,
			DiskMb:   s.Resources.DiskMB,
		},
		Replicas: &capsulePb.ReplicaConfig{
			Min:   s.Replicas.Min,
			Max:   s.Replicas.Max,
			Exact: s.Replicas.Exact,
		},
		Runtime: &capsulePb.RuntimeConfig{
			Env: s.Runtime.Env,
		},
		MomentumConfig: &capsulePb.MomentumConfig{
			Base:               s.MomentumConfig.Base,
			BoostOnTraffic:     s.MomentumConfig.BoostOnTraffic,
			ReduceOnIdle:       s.MomentumConfig.ReduceOnIdle,
			IdleTimeoutSeconds: int32(s.MomentumConfig.IdleTimeout.Seconds()),
		},
	}

	for _, rule := range s.ScalingRules {
		pb.ScalingRules = append(pb.ScalingRules, &capsulePb.ScalingRule{
			Name:            rule.Name,
			Trigger:         string(rule.Trigger),
			Conditions:      rule.Conditions,
			Action:          string(rule.Action),
			CooldownSeconds: int32(rule.Cooldown.Seconds()),
		})
	}

	for _, rule := range s.PlacementRules {
		pb.PlacementRules = append(pb.PlacementRules, &capsulePb.PlacementRule{
			Name:        rule.Name,
			Type:        string(rule.Type),
			Mode:        string(rule.Mode),
			TargetNames: rule.Names,
			Labels:      rule.Labels,
			Required:    rule.Required,
		})
	}

	return pb
}

func replicaStatesToProto(replicas []capsule.ReplicaState) []*capsulePb.ReplicaState {
	var result []*capsulePb.ReplicaState
	for _, r := range replicas {
		result = append(result, &capsulePb.ReplicaState{
			ReplicaId: string(r.ReplicaID),
			NodeId:    r.NodeID,
			Status:    string(r.Status),
			StartedAt: timestamppb.New(r.StartedAt),
		})
	}
	return result
}

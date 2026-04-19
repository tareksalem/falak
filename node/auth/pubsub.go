package auth

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/auth/certs"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/internal/signing"
	"github.com/tareksalem/falak/node/proto/authpb"
)

// authMessageLoop processes incoming auth PubSub messages.
func (a *Authenticator) authMessageLoop(ctx context.Context, clusterPath string, sub *pubsub.Subscription) {
	a.logger.Info("auth message loop started",
		zap.String("cluster", clusterPath),
		zap.String("selfID", a.host.ID().String()))

	for {
		a.logger.Debug("waiting for next pubsub message",
			zap.String("cluster", clusterPath))

		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				a.logger.Debug("auth message loop stopped (context cancelled)",
					zap.String("cluster", clusterPath))
				return // Context cancelled
			}
			a.logger.Error("error reading auth message", zap.Error(err))
			continue
		}

		a.logger.Debug("received pubsub message",
			zap.String("cluster", clusterPath),
			zap.String("from", msg.ReceivedFrom.String()),
			zap.Int("size", len(msg.Data)),
			zap.String("selfID", a.host.ID().String()))

		// Skip messages from ourselves
		if msg.ReceivedFrom == a.host.ID() {
			a.logger.Debug("skipping own message",
				zap.String("cluster", clusterPath))
			continue
		}

		a.handleAuthPubSubMessage(clusterPath, msg.Data)
	}
}

// handleAuthPubSubMessage processes a single auth PubSub message.
func (a *Authenticator) handleAuthPubSubMessage(clusterPath string, data []byte) {
	// Check for duplicate messages (replay protection within MaxMessageAge window)
	if a.messageDedup != nil && a.messageDedup.IsDuplicate(data) {
		a.logger.Debug("rejecting duplicate auth message",
			zap.String("cluster", clusterPath))
		return
	}

	var envelope authpb.AuthMessage
	if err := proto.Unmarshal(data, &envelope); err != nil {
		a.logger.Error("failed to unmarshal auth message", zap.Error(err))
		return
	}

	// Verify message age (replay protection)
	msgTime := envelope.Timestamp.AsTime()
	if time.Since(msgTime) > MaxMessageAge {
		a.logger.Debug("rejecting old auth message",
			zap.String("sender", envelope.SenderId),
			zap.Duration("age", time.Since(msgTime)))
		return
	}

	// Verify envelope signature: every message must be signed by a known sender
	if envelope.Signature == nil {
		a.logger.Warn("rejecting unsigned auth message",
			zap.String("sender", envelope.SenderId),
			zap.String("type", envelope.Type))
		return
	}

	senderPubKey, err := a.lookupPublicKey(envelope.SenderId, clusterPath)
	if err != nil {
		// For new_member messages, the sender is the voucher who must be known.
		// For auth_confirmed/auth_rejected, the sender must also be a known cluster member.
		a.logger.Warn("rejecting auth message from unknown sender",
			zap.String("sender", envelope.SenderId),
			zap.String("type", envelope.Type),
			zap.Error(err))
		return
	}

	if !a.verifyMessageSignature(&envelope, senderPubKey) {
		a.logger.Warn("rejecting auth message with invalid envelope signature",
			zap.String("sender", envelope.SenderId),
			zap.String("type", envelope.Type))
		return
	}

	switch envelope.Type {
	case "new_member":
		a.handleNewMemberAnnouncement(clusterPath, &envelope)
	case "auth_confirmed":
		a.handleAuthConfirmed(clusterPath, &envelope)
	case "auth_rejected":
		a.handleAuthRejected(clusterPath, &envelope)
	case "cert_revoked":
		a.handleCertRevoked(clusterPath, &envelope)
	default:
		a.logger.Debug("unknown auth message type", zap.String("type", envelope.Type))
	}
}

// handleNewMemberAnnouncement processes a new member announcement.
func (a *Authenticator) handleNewMemberAnnouncement(clusterPath string, envelope *authpb.AuthMessage) {
	a.logger.Info("processing new member announcement",
		zap.String("cluster", clusterPath),
		zap.String("sender", envelope.SenderId))

	var announcement authpb.NewMemberAnnouncement
	if err := proto.Unmarshal(envelope.Payload, &announcement); err != nil {
		a.logger.Error("failed to unmarshal new member announcement", zap.Error(err))
		return
	}

	a.logger.Debug("new member announcement details",
		zap.String("newNodeId", announcement.NewNodeId),
		zap.String("voucher", announcement.VoucherNodeId),
		zap.String("cluster", announcement.ClusterPath),
		zap.Bool("hasCertificate", announcement.Certificate != nil))

	// Test-only: reject every announcement when rejectAllAuth is set.
	if a.rejectAllAuth {
		a.logger.Warn("TEST FLAG: rejecting auth due to RejectAllAuth",
			zap.String("newNode", announcement.NewNodeId))
		a.publishAuthRejection(clusterPath, announcement.NewNodeId, "rejection forced by test flag")
		return
	}

	// Verify the announcement is for this cluster
	if announcement.ClusterPath != clusterPath {
		a.logger.Warn("cluster path mismatch in announcement",
			zap.String("expected", clusterPath),
			zap.String("got", announcement.ClusterPath))
		a.publishAuthRejection(clusterPath, announcement.NewNodeId, "cluster path mismatch")
		return
	}

	// Check if the voucher node has been revoked
	if a.IsNodeRevoked(clusterPath, announcement.VoucherNodeId) {
		a.logger.Warn("voucher node has been revoked",
			zap.String("voucher", announcement.VoucherNodeId),
			zap.String("newNode", announcement.NewNodeId))
		a.publishAuthRejection(clusterPath, announcement.NewNodeId, "voucher node certificate is revoked")
		return
	}

	// Look up the voucher's public key from phonebook
	voucherPubKey, err := a.lookupPublicKey(announcement.VoucherNodeId, clusterPath)
	if err != nil {
		a.logger.Warn("unknown voucher node",
			zap.String("voucher", announcement.VoucherNodeId),
			zap.Error(err))
		a.publishAuthRejection(clusterPath, announcement.NewNodeId, "unknown voucher node")
		return
	}

	// Build content to verify using canonical signing (same as what was signed by voucher)
	contentToVerify := AnnouncementContent(announcement.NewNodeId, announcement.ClusterPath, announcement.NewNodeAddrs, announcement.PublicKey)

	// Verify voucher signature over the announcement content
	ok, err := voucherPubKey.Verify(contentToVerify, announcement.VoucherSignature)
	if err != nil || !ok {
		a.logger.Warn("invalid voucher signature on new member announcement",
			zap.String("voucher", announcement.VoucherNodeId),
			zap.String("newNode", announcement.NewNodeId))
		a.publishAuthRejection(clusterPath, announcement.NewNodeId, "invalid voucher signature")
		return
	}

	// Verify certificate if present — use provider for mode-agnostic verification
	if announcement.Certificate != nil {
		provider, providerOK := a.GetProvider(clusterPath)
		if providerOK {
			cert := &certs.Certificate{NodeCertificate: announcement.Certificate}
			valid, verifyErr := provider.VerifyNodeCertificate(cert)
			if verifyErr != nil || !valid {
				a.logger.Warn("provider rejected certificate in announcement",
					zap.String("newNode", announcement.NewNodeId),
					zap.Error(verifyErr))
				a.publishAuthRejection(clusterPath, announcement.NewNodeId, "invalid certificate")
				return
			}
		} else {
			// Fallback: verify directly against voucher key (no provider initialized)
			certValid, certErr := certs.VerifyCertificateInAnnouncement(&announcement, voucherPubKey)
			if certErr != nil || !certValid {
				a.logger.Warn("invalid certificate in announcement",
					zap.String("newNode", announcement.NewNodeId),
					zap.Error(certErr))
				a.publishAuthRejection(clusterPath, announcement.NewNodeId, "invalid certificate")
				return
			}
		}
		a.logger.Debug("certificate verified successfully",
			zap.String("newNode", announcement.NewNodeId))
	}

	// All verifications passed - publish confirmation
	a.publishAuthConfirmation(clusterPath, announcement.NewNodeId)

	// Convert capabilities if present
	var caps *events.Capabilities
	if announcement.Capabilities != nil {
		caps = &events.Capabilities{
			CPUCores:   announcement.Capabilities.CpuCores,
			MemoryMB:   announcement.Capabilities.MemoryMb,
			DiskGB:     announcement.Capabilities.DiskGb,
			Datacenter: announcement.Capabilities.Datacenter,
			Tags:       announcement.Capabilities.Tags,
			Metadata:   announcement.Capabilities.Metadata,
		}
	}

	// Emit event - phonebook and other components will react
	a.eventBus.Publish(events.NewMemberReceived{
		BaseEvent:     events.NewBaseEvent(),
		NodeID:        announcement.NewNodeId,
		ClusterPath:   clusterPath,
		Addresses:     announcement.NewNodeAddrs,
		PublicKey:     announcement.PublicKey,
		Capabilities:  caps,
		VoucherNodeID: announcement.VoucherNodeId,
		JoinedAt:      announcement.Timestamp,
	})

	a.logger.Info("received new cluster member announcement",
		zap.String("nodeId", announcement.NewNodeId),
		zap.String("cluster", clusterPath),
		zap.String("voucher", announcement.VoucherNodeId))
}

// publishAuthConfirmation publishes a MemberAuthConfirmed message.
func (a *Authenticator) publishAuthConfirmation(clusterPath, newNodeID string) {
	confirmation := &authpb.MemberAuthConfirmed{
		NewNodeId:        newNodeID,
		ConfirmingNodeId: a.host.ID().String(),
		ClusterPath:      clusterPath,
		Timestamp:        timestamppb.Now(),
	}

	// Sign the confirmation using canonical encoding
	buf := signing.NewBuffer(128)
	buf.WriteString(newNodeID)
	buf.WriteString(clusterPath)
	tsBytes, _ := confirmation.Timestamp.AsTime().MarshalBinary()
	buf.WriteTimeBinary(tsBytes)
	contentToSign := buf.Bytes()

	sig, err := a.privateKey.Sign(contentToSign)
	if err != nil {
		a.logger.Error("failed to sign auth confirmation", zap.Error(err))
		return
	}
	confirmation.Signature = sig

	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()

	if err := a.publishAuthMessage(ctx, clusterPath, "auth_confirmed", confirmation); err != nil {
		a.logger.Error("failed to publish auth confirmation",
			zap.String("newNode", newNodeID),
			zap.Error(err))
	} else {
		a.logger.Info("published auth confirmation",
			zap.String("newNode", newNodeID),
			zap.String("cluster", clusterPath))
	}
}

// publishAuthRejection publishes a MemberAuthRejected message.
func (a *Authenticator) publishAuthRejection(clusterPath, newNodeID, reason string) {
	rejection := &authpb.MemberAuthRejected{
		NewNodeId:       newNodeID,
		RejectingNodeId: a.host.ID().String(),
		ClusterPath:     clusterPath,
		Reason:          reason,
		Timestamp:       timestamppb.Now(),
	}

	// Sign the rejection using canonical encoding
	rejBuf := signing.NewBuffer(128)
	rejBuf.WriteString(newNodeID)
	rejBuf.WriteString(clusterPath)
	rejBuf.WriteString(reason)
	rejTsBytes, _ := rejection.Timestamp.AsTime().MarshalBinary()
	rejBuf.WriteTimeBinary(rejTsBytes)
	contentToSign := rejBuf.Bytes()

	sig, err := a.privateKey.Sign(contentToSign)
	if err != nil {
		a.logger.Error("failed to sign auth rejection", zap.Error(err))
		return
	}
	rejection.Signature = sig

	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()

	if err := a.publishAuthMessage(ctx, clusterPath, "auth_rejected", rejection); err != nil {
		a.logger.Error("failed to publish auth rejection",
			zap.String("newNode", newNodeID),
			zap.Error(err))
	} else {
		a.logger.Warn("published auth rejection",
			zap.String("newNode", newNodeID),
			zap.String("cluster", clusterPath),
			zap.String("reason", reason))
	}
}

// handleAuthConfirmed processes an auth confirmed message.
func (a *Authenticator) handleAuthConfirmed(clusterPath string, envelope *authpb.AuthMessage) {
	var confirmation authpb.MemberAuthConfirmed
	if err := proto.Unmarshal(envelope.Payload, &confirmation); err != nil {
		a.logger.Error("failed to unmarshal auth confirmed", zap.Error(err))
		return
	}

	a.logger.Info("received auth confirmation",
		zap.String("newNode", confirmation.NewNodeId),
		zap.String("confirmer", confirmation.ConfirmingNodeId),
		zap.String("cluster", clusterPath))

	// Verify signature
	confirmerPubKey, err := a.lookupPublicKey(confirmation.ConfirmingNodeId, clusterPath)
	if err != nil {
		a.logger.Warn("unknown confirming node", zap.Error(err))
		return
	}

	confirmBuf := signing.NewBuffer(128)
	confirmBuf.WriteString(confirmation.NewNodeId)
	confirmBuf.WriteString(clusterPath)
	confirmTsBytes, _ := confirmation.Timestamp.AsTime().MarshalBinary()
	confirmBuf.WriteTimeBinary(confirmTsBytes)

	ok, err := confirmerPubKey.Verify(confirmBuf.Bytes(), confirmation.Signature)
	if err != nil || !ok {
		a.logger.Warn("invalid signature on auth confirmation",
			zap.String("confirmer", confirmation.ConfirmingNodeId))
		return
	}

	// Update pending auth if we're tracking it (we're the voucher)
	pendingKey := confirmation.NewNodeId + ":" + clusterPath
	a.pendingMu.Lock()
	if pending, ok := a.pendingAuth[pendingKey]; ok {
		pending.Confirmations[confirmation.ConfirmingNodeId] = true

		// Check if we have enough confirmations (first-wins)
		if len(pending.Confirmations) >= MinConfirmationsRequired && len(pending.Rejections) == 0 {
			select {
			case pending.Done <- AuthResult{Confirmed: true}:
			default:
			}
		}
	}
	a.pendingMu.Unlock()
}

// handleAuthRejected processes an auth rejected message.
func (a *Authenticator) handleAuthRejected(clusterPath string, envelope *authpb.AuthMessage) {
	var rejection authpb.MemberAuthRejected
	if err := proto.Unmarshal(envelope.Payload, &rejection); err != nil {
		a.logger.Error("failed to unmarshal auth rejected", zap.Error(err))
		return
	}

	a.logger.Warn("received auth rejection",
		zap.String("newNode", rejection.NewNodeId),
		zap.String("rejecter", rejection.RejectingNodeId),
		zap.String("cluster", clusterPath),
		zap.String("reason", rejection.Reason))

	// Verify signature
	rejecterPubKey, err := a.lookupPublicKey(rejection.RejectingNodeId, clusterPath)
	if err != nil {
		a.logger.Warn("unknown rejecting node", zap.Error(err))
		return
	}

	rejectBuf := signing.NewBuffer(128)
	rejectBuf.WriteString(rejection.NewNodeId)
	rejectBuf.WriteString(clusterPath)
	rejectBuf.WriteString(rejection.Reason)
	rejectTsBytes, _ := rejection.Timestamp.AsTime().MarshalBinary()
	rejectBuf.WriteTimeBinary(rejectTsBytes)

	ok, err := rejecterPubKey.Verify(rejectBuf.Bytes(), rejection.Signature)
	if err != nil || !ok {
		a.logger.Warn("invalid signature on auth rejection",
			zap.String("rejecter", rejection.RejectingNodeId))
		return
	}

	// Update pending auth if we're tracking it (we're the voucher)
	// Rejection priority: any rejection immediately fails the auth
	pendingKey := rejection.NewNodeId + ":" + clusterPath
	a.pendingMu.Lock()
	if pending, ok := a.pendingAuth[pendingKey]; ok {
		pending.Rejections[rejection.RejectingNodeId] = rejection.Reason

		// Rejection priority - immediately fail
		select {
		case pending.Done <- AuthResult{Confirmed: false, Reason: rejection.Reason}:
		default:
		}
	}
	a.pendingMu.Unlock()
}

// handleCertRevoked processes a certificate revocation message.
func (a *Authenticator) handleCertRevoked(clusterPath string, envelope *authpb.AuthMessage) {
	var revocation authpb.CertificateRevocation
	if err := proto.Unmarshal(envelope.Payload, &revocation); err != nil {
		a.logger.Error("failed to unmarshal cert revocation", zap.Error(err))
		return
	}

	// Verify the revocation signature
	revokerPubKey, err := a.lookupPublicKey(revocation.RevokedBy, clusterPath)
	if err != nil {
		a.logger.Warn("unknown revoking node",
			zap.String("revokedBy", revocation.RevokedBy),
			zap.Error(err))
		return
	}

	buf := signing.NewBuffer(256)
	buf.WriteString(revocation.CertFingerprint)
	buf.WriteString(revocation.NodeId)
	buf.WriteString(revocation.ClusterPath)
	buf.WriteString(revocation.Reason)
	buf.WriteString(revocation.RevokedBy)
	if revocation.RevokedAt != nil {
		tsBytes, _ := revocation.RevokedAt.AsTime().MarshalBinary()
		buf.WriteTimeBinary(tsBytes)
	}

	ok, err := revokerPubKey.Verify(buf.Bytes(), revocation.Signature)
	if err != nil || !ok {
		a.logger.Warn("invalid signature on cert revocation",
			zap.String("revokedBy", revocation.RevokedBy))
		return
	}

	// Add to local revocation list
	rl := a.GetRevocationList(clusterPath)
	entry := certs.RevocationEntry{
		CertFingerprint: revocation.CertFingerprint,
		NodeID:          revocation.NodeId,
		ClusterPath:     revocation.ClusterPath,
		RevokedAt:       revocation.RevokedAt.AsTime(),
		Reason:          revocation.Reason,
		RevokedBy:       revocation.RevokedBy,
	}

	if err := rl.Revoke(entry); err != nil {
		a.logger.Error("failed to store revocation",
			zap.String("nodeId", revocation.NodeId),
			zap.Error(err))
		return
	}

	a.logger.Info("certificate revoked",
		zap.String("nodeId", revocation.NodeId),
		zap.String("cluster", clusterPath),
		zap.String("revokedBy", revocation.RevokedBy),
		zap.String("reason", revocation.Reason))
}

// AnnouncePresence broadcasts our presence to a cluster.
func (a *Authenticator) AnnouncePresence(ctx context.Context, clusterPath string) error {
	pubKeyBytes, err := crypto.MarshalPublicKey(a.privateKey.GetPublic())
	if err != nil {
		return err
	}

	addrs := make([]string, 0, len(a.host.Addrs()))
	for _, addr := range a.host.Addrs() {
		addrs = append(addrs, addr.String())
	}

	// Sign the announcement content using canonical encoding (self-vouching)
	contentToSign := AnnouncementContent(a.host.ID().String(), clusterPath, addrs, pubKeyBytes)
	selfSig, err := a.privateKey.Sign(contentToSign)
	if err != nil {
		return err
	}

	announcement := &authpb.NewMemberAnnouncement{
		NewNodeId:        a.host.ID().String(),
		NewNodeAddrs:     addrs,
		ClusterPath:      clusterPath,
		PublicKey:        pubKeyBytes,
		VoucherNodeId:    a.host.ID().String(), // Self-vouching for presence
		VoucherSignature: selfSig,
		Timestamp:        timestamppb.Now(),
	}

	return a.publishAuthMessage(ctx, clusterPath, "new_member", announcement)
}

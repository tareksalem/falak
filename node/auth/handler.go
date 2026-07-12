package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/auth/certs"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/internal/signing"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/authpb"
	"github.com/tareksalem/falak/shared"
)

// isTransientStreamErr reports whether err is a benign mid-handshake
// stream interruption: the peer closed the stream cleanly (io.EOF),
// libp2p reset it ("stream reset"), the OS reset the connection
// ("connection reset by peer"), or our own context was cancelled. These
// happen routinely during gossipsub mesh churn and operator-driven
// restarts; logging them at ERROR with stacks just adds noise (Bug #11).
func isTransientStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "stream reset") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "broken pipe")
}

// logStreamError emits err at Debug for transient stream interruptions
// and at Error for genuine failures. msg/peer are passed through verbatim
// so call sites read naturally.
func (a *Authenticator) logStreamError(msg, peer string, err error) {
	if isTransientStreamErr(err) {
		a.logger.Debug(msg,
			zap.String("peer", peer),
			zap.Error(err))
		return
	}
	a.logger.Error(msg,
		zap.String("peer", peer),
		zap.Error(err))
}

// handleAuthStream handles incoming authentication requests (Step 1).
func (a *Authenticator) handleAuthStream(stream network.Stream) {
	defer stream.Close()

	remotePeer := stream.Conn().RemotePeer()
	a.logger.Debug("handling auth stream", zap.String("peer", remotePeer.String()))

	// Check rate limit
	if !a.rateLimiter.Allow(remotePeer.String()) {
		a.logger.Warn("rate limited auth request",
			zap.String("peer", remotePeer.String()))
		a.sendStep1Result(stream, false, nil, "rate limited")
		return
	}

	// Read JoinRequest
	var joinReq authpb.JoinRequest
	if err := shared.ReadProto(stream, &joinReq); err != nil {
		a.logStreamError("failed to read join request", remotePeer.String(), err)
		return
	}

	a.logger.Info("received join request, validating PSK",
		zap.String("peer", joinReq.NodeId),
		zap.String("cluster", joinReq.ClusterPath))

	// Validate cluster HMAC key exists (derived from PSK at cluster setup)
	a.hmacKeyMu.RLock()
	hmacKey, ok := a.hmacKeys[joinReq.ClusterPath]
	a.hmacKeyMu.RUnlock()

	if !ok {
		a.logger.Warn("unknown cluster", zap.String("cluster", joinReq.ClusterPath))
		a.sendStep1Result(stream, false, nil, "unknown cluster")
		return
	}

	// Check if the joining node has a revoked certificate
	if a.IsNodeRevoked(joinReq.ClusterPath, joinReq.NodeId) {
		a.logger.Warn("rejecting auth from revoked node",
			zap.String("peer", joinReq.NodeId),
			zap.String("cluster", joinReq.ClusterPath))
		a.sendStep1Result(stream, false, nil, "node certificate has been revoked")
		return
	}

	// Generate server nonce
	serverNonce := make([]byte, 32)
	if _, err := rand.Read(serverNonce); err != nil {
		a.logger.Error("failed to generate server nonce", zap.Error(err))
		a.sendStep1Result(stream, false, nil, "internal error")
		return
	}

	// Generate challenge
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		a.logger.Error("failed to generate challenge", zap.Error(err))
		a.sendStep1Result(stream, false, nil, "internal error")
		return
	}

	// Send Challenge
	challengeMsg := &authpb.Challenge{
		Challenge:   challenge,
		ServerNonce: serverNonce,
	}
	if err := shared.WriteProto(stream, challengeMsg); err != nil {
		a.logStreamError("failed to send challenge", remotePeer.String(), err)
		return
	}

	// Read ChallengeResponse
	var challengeResp authpb.ChallengeResponse
	if err := shared.ReadProto(stream, &challengeResp); err != nil {
		a.logStreamError("failed to read challenge response", remotePeer.String(), err)
		return
	}

	// Verify HMAC response
	expectedData := append(challenge, joinReq.Nonce...)
	expectedData = append(expectedData, serverNonce...)

	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(expectedData)
	expectedResponse := mac.Sum(nil)

	if !hmac.Equal(challengeResp.Response, expectedResponse) {
		a.logger.Warn("invalid HMAC response", zap.String("peer", joinReq.NodeId))
		a.sendStep1Result(stream, false, nil, "authentication failed")
		return
	}

	// Verify signature
	pubKey, err := crypto.UnmarshalPublicKey(joinReq.PublicKey)
	if err != nil {
		a.logger.Warn("invalid public key", zap.Error(err))
		a.sendStep1Result(stream, false, nil, "invalid public key")
		return
	}

	ok, err = pubKey.Verify(challengeResp.Response, challengeResp.Signature)
	if err != nil || !ok {
		a.logger.Warn("invalid signature", zap.String("peer", joinReq.NodeId))
		a.sendStep1Result(stream, false, nil, "invalid signature")
		return
	}

	a.logger.Info("PSK validated, signing certificate",
		zap.String("peer", joinReq.NodeId),
		zap.String("cluster", joinReq.ClusterPath))

	// Send Step1Result success
	a.sendStep1Result(stream, true, nil, "")

	// Convert capabilities if present
	var caps *events.Capabilities
	if joinReq.Capabilities != nil {
		caps = &events.Capabilities{
			CPUCores:   joinReq.Capabilities.CpuCores,
			MemoryMB:   joinReq.Capabilities.MemoryMb,
			DiskGB:     joinReq.Capabilities.DiskGb,
			Datacenter: joinReq.Capabilities.Datacenter,
			Tags:       joinReq.Capabilities.Tags,
			Metadata:   joinReq.Capabilities.Metadata,
		}
	}

	// Emit event for new member - phonebook and other components will react
	a.eventBus.Publish(events.NewMemberAnnounced{
		BaseEvent:    events.NewBaseEvent(),
		NodeID:       joinReq.NodeId,
		ClusterPath:  joinReq.ClusterPath,
		Addresses:    joinReq.Addresses,
		PublicKey:    joinReq.PublicKey,
		Capabilities: caps,
	})

	// Build Step 2 announcement with certificate
	announcement, err := a.buildNewMemberAnnouncement(joinReq.NodeId, joinReq.ClusterPath, joinReq.Addresses, joinReq.PublicKey, joinReq.Capabilities)
	if err != nil {
		a.logger.Error("failed to build announcement, aborting auth",
			zap.String("peer", joinReq.NodeId),
			zap.Error(err))
		authComplete := &authpb.AuthComplete{
			FullyAuthenticated: false,
		}
		shared.WriteProto(stream, authComplete)
		return
	}

	// Broadcast new member announcement (Step 2) with retries
	ctx, cancel := context.WithTimeout(a.ctx, AuthTimeout)
	defer cancel()

	// Count third-party confirmers — peers other than the voucher
	// (self) and the joining node. The PKI confirmation handshake
	// only makes sense when at least one *other* peer can verify the
	// new cert. On a 2-node bootstrap (just self + joiner) the wait
	// would burn the full ConfirmationTimeout for nothing — turning
	// joins from ~50ms into ~15s.
	var clusterMemberCount int
	if a.phonebook != nil {
		entries, _ := a.phonebook.GetByCluster(joinReq.ClusterPath)
		selfID := a.host.ID().String()
		for _, e := range entries {
			if e.NodeID == selfID || e.NodeID == joinReq.NodeId {
				continue
			}
			clusterMemberCount++
		}
	}

	// Only wait for confirmations if we have other members in the cluster
	if clusterMemberCount > 0 {
		// Create pending auth tracker
		pendingKey := joinReq.NodeId + ":" + joinReq.ClusterPath
		pending := &PendingAuth{
			NodeID:        joinReq.NodeId,
			ClusterPath:   joinReq.ClusterPath,
			Certificate:   &certs.Certificate{NodeCertificate: announcement.Certificate},
			Confirmations: make(map[string]bool),
			Rejections:    make(map[string]string),
			Done:          make(chan AuthResult, 1),
			StartedAt:     time.Now(),
		}

		a.pendingMu.Lock()
		a.pendingAuth[pendingKey] = pending
		a.pendingMu.Unlock()

		// Clean up pending auth when done
		defer func() {
			a.pendingMu.Lock()
			delete(a.pendingAuth, pendingKey)
			a.pendingMu.Unlock()
		}()

		// Broadcast the announcement
		if err := a.publishStep2(ctx, joinReq.ClusterPath, announcement); err != nil {
			a.logger.Error("step 2 broadcast failed after retries",
				zap.Error(err),
				zap.String("peer", joinReq.NodeId))
			authComplete := &authpb.AuthComplete{
				FullyAuthenticated: false,
			}
			shared.WriteProto(stream, authComplete)
			return
		}

		// Wait for confirmations or timeout
		confirmCtx, confirmCancel := context.WithTimeout(ctx, ConfirmationTimeout)
		defer confirmCancel()

		select {
		case result := <-pending.Done:
			if !result.Confirmed {
				a.logger.Warn("authentication rejected by cluster",
					zap.String("peer", joinReq.NodeId),
					zap.String("reason", result.Reason))
				authComplete := &authpb.AuthComplete{
					FullyAuthenticated: false,
				}
				shared.WriteProto(stream, authComplete)
				return
			}
			a.logger.Info("authentication confirmed by cluster",
				zap.String("peer", joinReq.NodeId))
		case <-confirmCtx.Done():
			// Timeout - check if we have any confirmations or rejections
			a.pendingMu.RLock()
			confirmCount := len(pending.Confirmations)
			rejectCount := len(pending.Rejections)
			a.pendingMu.RUnlock()

			// If we have any rejections, reject the auth (rejection priority)
			if rejectCount > 0 {
				a.logger.Warn("authentication rejected (timeout with rejections)",
					zap.String("peer", joinReq.NodeId),
					zap.Int("rejections", rejectCount))
				authComplete := &authpb.AuthComplete{
					FullyAuthenticated: false,
				}
				shared.WriteProto(stream, authComplete)
				return
			}

			// If we have at least one confirmation, proceed
			if confirmCount >= MinConfirmationsRequired {
				a.logger.Info("authentication confirmed (partial quorum)",
					zap.String("peer", joinReq.NodeId),
					zap.Int("confirmations", confirmCount))
			} else {
				// No confirmations and no rejections within timeout - allow if we're the only other node
				a.logger.Info("authentication proceeding (no responses, voucher-only)",
					zap.String("peer", joinReq.NodeId))
			}
		}
	} else {
		// No other cluster members - just broadcast and proceed
		if err := a.publishStep2(ctx, joinReq.ClusterPath, announcement); err != nil {
			a.logger.Error("step 2 broadcast failed after retries",
				zap.Error(err),
				zap.String("peer", joinReq.NodeId))
			authComplete := &authpb.AuthComplete{
				FullyAuthenticated: false,
			}
			shared.WriteProto(stream, authComplete)
			return
		}
	}

	// Get cluster members from phonebook for the joining node
	var members []*authpb.MemberInfo
	if a.phonebook != nil {
		members = a.getClusterMembers(joinReq.ClusterPath)
	}

	// Send AuthComplete with success, including the certificate we created for the joining node
	authComplete := &authpb.AuthComplete{
		FullyAuthenticated: true,
		ClusterMembers:     members,
		NodeCertificate:    announcement.Certificate,
	}
	if err := shared.WriteProto(stream, authComplete); err != nil {
		a.logStreamError("failed to send auth complete", joinReq.NodeId, err)
		return
	}

	// Flip the joiner's phonebook entry from PendingAuth to Active now
	// that AuthComplete has been delivered. Without this the SWIM grace
	// window would do it eventually (Bug #13 default 10s) — but we know
	// the handshake succeeded, so we don't need to wait.
	if a.phonebook != nil {
		if err := a.phonebook.SetStatus(joinReq.NodeId, joinReq.ClusterPath, phonebook.NodeStatusEnum.Active()); err != nil {
			a.logger.Debug("failed to promote joiner to active",
				zap.String("peer", joinReq.NodeId),
				zap.Error(err))
		}
	}

	a.logger.Info("authenticated new member",
		zap.String("peer", joinReq.NodeId),
		zap.String("cluster", joinReq.ClusterPath))

	// Emit MemberAdmitted so the syncer can actively push this new member to
	// every existing Active peer (Layer 1 of the O13 join-convergence fix).
	// This is an event-driven seam — auth never calls the syncer directly.
	// We carry the full member info the JoinRequest already gave us so the
	// syncer builds the push without a phonebook re-read that would race the
	// joiner's own Add. JoinedAt is stamped now (the moment of admission).
	a.eventBus.Publish(events.MemberAdmitted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: joinReq.ClusterPath,
		NewMember: events.MemberInfo{
			NodeID:       joinReq.NodeId,
			Addresses:    joinReq.Addresses,
			PublicKey:    joinReq.PublicKey,
			Capabilities: caps,
			JoinedAt:     timestamppb.Now(),
		},
	})
}

// sendStep1Result sends a Step1Result message.
func (a *Authenticator) sendStep1Result(stream network.Stream, success bool, token []byte, errMsg string) {
	result := &authpb.Step1Result{
		Success:    success,
		Step1Token: token,
		Error:      errMsg,
	}
	if err := shared.WriteProto(stream, result); err != nil {
		peerStr := ""
		if stream != nil {
			peerStr = stream.Conn().RemotePeer().String()
		}
		a.logStreamError("failed to send step1 result", peerStr, err)
	}
}

// AnnouncementContent builds the canonical bytes for signing/verifying a NewMemberAnnouncement.
// This is used by both the voucher (signing) and receiving nodes (verification).
func AnnouncementContent(nodeID, clusterPath string, addresses []string, publicKey []byte) []byte {
	buf := signing.NewBuffer(256)
	buf.WriteString(nodeID)
	buf.WriteString(clusterPath)
	buf.WriteStrings(addresses)
	buf.WriteField(publicKey)
	return buf.Bytes()
}

// buildNewMemberAnnouncement creates a signed new member announcement with certificate.
// Returns an error if signing or certificate creation fails — the caller must abort the auth flow.
func (a *Authenticator) buildNewMemberAnnouncement(nodeID, clusterPath string, addresses []string, publicKey []byte, caps *authpb.Capabilities) (*authpb.NewMemberAnnouncement, error) {
	contentToSign := AnnouncementContent(nodeID, clusterPath, addresses, publicKey)

	voucherSig, err := a.privateKey.Sign(contentToSign)
	if err != nil {
		return nil, fmt.Errorf("failed to sign announcement: %w", err)
	}

	announcement := &authpb.NewMemberAnnouncement{
		NewNodeId:        nodeID,
		NewNodeAddrs:     addresses,
		ClusterPath:      clusterPath,
		PublicKey:        publicKey,
		Capabilities:     caps,
		VoucherNodeId:    a.host.ID().String(),
		VoucherSignature: voucherSig,
		Timestamp:        timestamppb.Now(),
	}

	// Create certificate for the new node via the provider
	nodePubKey, err := crypto.UnmarshalPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal node public key for certificate: %w", err)
	}

	provider, ok := a.GetProvider(clusterPath)
	if !ok {
		return nil, fmt.Errorf("no certificate provider for cluster %s", clusterPath)
	}

	if !provider.CanSign() {
		return nil, fmt.Errorf("certificate provider for cluster %s cannot sign (no CA key)", clusterPath)
	}

	cert, err := provider.SignForNode(nodeID, nodePubKey, clusterPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate for new node: %w", err)
	}

	announcement.Certificate = cert.NodeCertificate

	a.logger.Debug("created certificate for new member",
		zap.String("nodeId", nodeID),
		zap.String("cluster", clusterPath),
		zap.String("voucher", a.host.ID().String()))

	return announcement, nil
}

// getClusterMembers returns all members in a cluster as MemberInfo.
func (a *Authenticator) getClusterMembers(clusterPath string) []*authpb.MemberInfo {
	entries, err := a.phonebook.GetByCluster(clusterPath)
	if err != nil {
		a.logger.Error("failed to list cluster members", zap.Error(err))
		return nil
	}

	members := make([]*authpb.MemberInfo, 0, len(entries))
	for _, entry := range entries {
		member := &authpb.MemberInfo{
			NodeId:    entry.NodeID,
			Addresses: entry.Addresses,
			PublicKey: entry.PublicKey,
			JoinedAt:  timestamppb.New(entry.FirstSeen),
		}

		if entry.Capabilities != nil {
			member.Capabilities = &authpb.Capabilities{
				CpuCores:   entry.Capabilities.CPUCores,
				MemoryMb:   entry.Capabilities.MemoryMB,
				DiskGb:     entry.Capabilities.DiskGB,
				Datacenter: entry.Capabilities.Datacenter,
				Tags:       entry.Capabilities.Tags,
				Metadata:   entry.Capabilities.Metadata,
			}
		}

		members = append(members, member)
	}

	return members
}

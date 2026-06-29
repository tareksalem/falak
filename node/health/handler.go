package health

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/proto/healthpb"
	"github.com/tareksalem/falak/shared"
)

const (
	// PingProtocolID is the libp2p protocol for health pings.
	PingProtocolID = "/falak/health/ping/1.0"

	// DefaultPingTimeout is the maximum time to wait for a ping/ack exchange.
	DefaultPingTimeout = 500 * time.Millisecond
)

// PingHandler handles incoming and outgoing health pings.
type PingHandler struct {
	host        host.Host
	logger      *zap.Logger
	pingTimeout time.Duration
}

// NewPingHandler creates a new ping handler.
func NewPingHandler(h host.Host, logger *zap.Logger, pingTimeout time.Duration) *PingHandler {
	return &PingHandler{
		host:        h,
		logger:      logger,
		pingTimeout: pingTimeout,
	}
}

// Register registers the ping stream handler on the libp2p host.
func (ph *PingHandler) Register() {
	ph.host.SetStreamHandler(PingProtocolID, ph.handlePingStream)
}

// Unregister removes the ping stream handler.
func (ph *PingHandler) Unregister() {
	ph.host.RemoveStreamHandler(PingProtocolID)
}

// handlePingStream handles an incoming ping request from a remote peer.
// Reads a Ping message and responds with an Ack containing the matching sequence.
func (ph *PingHandler) handlePingStream(stream network.Stream) {
	defer stream.Close()
	remotePeer := stream.Conn().RemotePeer().String()

	// Set read deadline
	stream.SetReadDeadline(time.Now().Add(ph.pingTimeout))

	var ping healthpb.Ping
	if err := shared.ReadProto(stream, &ping); err != nil {
		ph.logger.Debug("failed to read ping",
			zap.String("peer", remotePeer),
			zap.Error(err))
		return
	}

	ph.logger.Debug("received ping from peer",
		zap.String("peer", remotePeer),
		zap.Int64("sequence", ping.Sequence))

	// Respond with ack matching the sequence number
	ack := &healthpb.Ack{
		FromNodeId: ph.host.ID().String(),
		Sequence:   ping.Sequence,
		Timestamp:  timestamppb.Now(),
	}

	stream.SetWriteDeadline(time.Now().Add(ph.pingTimeout))
	if err := shared.WriteProto(stream, ack); err != nil {
		ph.logger.Debug("failed to write ack",
			zap.String("peer", remotePeer),
			zap.Error(err))
		return
	}

	ph.logger.Debug("sent ack to peer",
		zap.String("peer", remotePeer),
		zap.Int64("sequence", ping.Sequence))
}

// Ping sends a direct ping to a target peer and waits for an ack.
// Returns true if the target responded with a matching sequence, false otherwise.
func (ph *PingHandler) Ping(ctx context.Context, target peer.ID) bool {
	ok, _ := ph.PingWithError(ctx, target)
	return ok
}

// PingWithError sends a direct ping and returns the underlying error so
// the caller can distinguish transport failures (e.g. "dial to self
// attempted") from a missing ack. Used by the SWIM monitor to evict
// phonebook ghosts whose multiaddrs collide with the local node.
func (ph *PingHandler) PingWithError(ctx context.Context, target peer.ID) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ph.pingTimeout)
	defer cancel()

	ph.logger.Debug("sending ping to peer",
		zap.String("target", target.String()))

	stream, err := ph.host.NewStream(ctx, target, PingProtocolID)
	if err != nil {
		ph.logger.Debug("failed to open ping stream",
			zap.String("target", target.String()),
			zap.Error(err))
		return false, err
	}
	defer stream.Close()

	sequence := time.Now().UnixNano()

	ping := &healthpb.Ping{
		FromNodeId: ph.host.ID().String(),
		Sequence:   sequence,
		Timestamp:  timestamppb.Now(),
	}

	stream.SetWriteDeadline(time.Now().Add(ph.pingTimeout))
	if err := shared.WriteProto(stream, ping); err != nil {
		ph.logger.Debug("failed to write ping",
			zap.String("target", target.String()),
			zap.Error(err))
		return false, err
	}

	var ack healthpb.Ack
	stream.SetReadDeadline(time.Now().Add(ph.pingTimeout))
	if err := shared.ReadProto(stream, &ack); err != nil {
		ph.logger.Debug("failed to read ack",
			zap.String("target", target.String()),
			zap.Error(err))
		return false, err
	}

	// Verify sequence matches
	if ack.Sequence != sequence {
		ph.logger.Debug("ack sequence mismatch",
			zap.String("target", target.String()),
			zap.Int64("expected", sequence),
			zap.Int64("got", ack.Sequence))
		return false, nil
	}

	ph.logger.Debug("received ack from peer",
		zap.String("target", target.String()),
		zap.Int64("sequence", sequence))

	return true, nil
}

// IsDialToSelfError reports whether the libp2p dial error indicates that
// the target's resolved multiaddrs all match the local host's listen
// addresses. When true, the phonebook entry that produced the dial is a
// ghost left over from a prior node identity at the same address — the
// SWIM monitor should evict it directly rather than letting it drift
// through suspect/quarantine/fail.
func IsDialToSelfError(err error) bool {
	if err == nil {
		return false
	}
	// libp2p returns the literal substring "dial to self attempted" in
	// the error chain. Substring match is the only stable contract —
	// the error type is internal to go-libp2p-swarm.
	return contains(err.Error(), "dial to self attempted")
}

func contains(haystack, needle string) bool {
	// Local helper to avoid pulling strings just for one Contains call.
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

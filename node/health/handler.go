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

	// Set read deadline
	stream.SetReadDeadline(time.Now().Add(ph.pingTimeout))

	var ping healthpb.Ping
	if err := shared.ReadProto(stream, &ping); err != nil {
		ph.logger.Debug("failed to read ping",
			zap.String("peer", stream.Conn().RemotePeer().String()),
			zap.Error(err))
		return
	}

	// Respond with ack matching the sequence number
	ack := &healthpb.Ack{
		FromNodeId: ph.host.ID().String(),
		Sequence:   ping.Sequence,
		Timestamp:  timestamppb.Now(),
	}

	stream.SetWriteDeadline(time.Now().Add(ph.pingTimeout))
	if err := shared.WriteProto(stream, ack); err != nil {
		ph.logger.Debug("failed to write ack",
			zap.String("peer", stream.Conn().RemotePeer().String()),
			zap.Error(err))
	}
}

// Ping sends a direct ping to a target peer and waits for an ack.
// Returns true if the target responded with a matching sequence, false otherwise.
func (ph *PingHandler) Ping(ctx context.Context, target peer.ID) bool {
	ctx, cancel := context.WithTimeout(ctx, ph.pingTimeout)
	defer cancel()

	stream, err := ph.host.NewStream(ctx, target, PingProtocolID)
	if err != nil {
		ph.logger.Debug("failed to open ping stream",
			zap.String("target", target.String()),
			zap.Error(err))
		return false
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
		return false
	}

	var ack healthpb.Ack
	stream.SetReadDeadline(time.Now().Add(ph.pingTimeout))
	if err := shared.ReadProto(stream, &ack); err != nil {
		ph.logger.Debug("failed to read ack",
			zap.String("target", target.String()),
			zap.Error(err))
		return false
	}

	// Verify sequence matches
	if ack.Sequence != sequence {
		ph.logger.Debug("ack sequence mismatch",
			zap.String("target", target.String()),
			zap.Int64("expected", sequence),
			zap.Int64("got", ack.Sequence))
		return false
	}

	return true
}

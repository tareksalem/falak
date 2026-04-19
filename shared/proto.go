package shared

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"
)

const (
	// MaxMessageSize is the maximum allowed protobuf message size (10MB)
	MaxMessageSize = 10 * 1024 * 1024
)

// WriteProto writes a length-prefixed protobuf message to a writer.
// Format: [4-byte big-endian length][protobuf data]
func WriteProto(w io.Writer, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal proto: %w", err)
	}

	// Write 4-byte length prefix (big endian)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))

	if _, err := w.Write(lenBuf); err != nil {
		return fmt.Errorf("failed to write length prefix: %w", err)
	}

	// Write protobuf data
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("failed to write proto data: %w", err)
	}

	return nil
}

// ReadProto reads a length-prefixed protobuf message from a reader.
// Format: [4-byte big-endian length][protobuf data]
func ReadProto(r io.Reader, msg proto.Message) error {
	// Read 4-byte length prefix
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return fmt.Errorf("failed to read length prefix: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)

	// Validate message size
	if length > MaxMessageSize {
		return fmt.Errorf("message size %d exceeds maximum %d", length, MaxMessageSize)
	}

	// Read protobuf data
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("failed to read proto data: %w", err)
	}

	// Deserialize into message
	if err := proto.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("failed to unmarshal proto: %w", err)
	}

	return nil
}

// ReadProtoWithDeadline reads a length-prefixed protobuf message with a deadline.
// The reader must implement SetReadDeadline (e.g., network streams).
type DeadlineReader interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

func ReadProtoWithDeadline(r DeadlineReader, msg proto.Message, deadline time.Time) error {
	if err := r.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("failed to set read deadline: %w", err)
	}

	return ReadProto(r, msg)
}

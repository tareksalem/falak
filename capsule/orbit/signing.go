package orbit

import (
	"encoding/binary"
	"time"

	capsulePb "github.com/tareksalem/falak/capsule/proto/capsulepb"
)

// canonicalContent builds the canonical byte sequence to sign/verify for an
// orbit message envelope. Each field is length-prefixed with a 4-byte big-endian
// uint32 to prevent ambiguity from field concatenation.
//
// Format:
//
//	[len][type][len][payload][len][senderID][len][timestamp-nano-bytes]
func canonicalContent(msg *capsulePb.OrbitMessage) []byte {
	// Estimate capacity.
	capacity := 4 + len(msg.Type) + 4 + len(msg.Payload) + 4 + len(msg.SenderId) + 4 + 8
	buf := make([]byte, 0, capacity)

	buf = writeField(buf, []byte(msg.Type))
	buf = writeField(buf, msg.Payload)
	buf = writeField(buf, []byte(msg.SenderId))

	var tsBytes []byte
	if msg.Timestamp != nil {
		tsNano := msg.Timestamp.AsTime().UnixNano()
		tsBytes = make([]byte, 8)
		binary.BigEndian.PutUint64(tsBytes, uint64(tsNano))
	}
	buf = writeField(buf, tsBytes)

	return buf
}

// writeField appends [4-byte BE length][data] to buf and returns the new slice.
func writeField(buf, data []byte) []byte {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	return buf
}

// MaxMessageAge is the maximum allowed age for an orbit message before it is rejected as stale.
const MaxMessageAge = 30 * time.Second

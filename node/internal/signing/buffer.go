// Package signing provides a canonical signing buffer for cryptographic operations.
//
// The buffer eliminates ambiguity in byte concatenation by encoding each field
// as [4-byte big-endian length][field bytes]. This makes it impossible for
// different field combinations to produce identical byte sequences.
//
// Example: "ab"+"cd" and "a"+"bcd" produce different encodings:
//
//	[0,0,0,2]"ab"[0,0,0,2]"cd"  vs  [0,0,0,1]"a"[0,0,0,3]"bcd"
package signing

import (
	"encoding/binary"
)

// Buffer builds a canonical byte sequence for cryptographic signing.
// Each field is length-prefixed with a 4-byte big-endian uint32, ensuring
// that different field combinations always produce different byte sequences.
type Buffer struct {
	buf []byte
}

// NewBuffer creates a new signing buffer with the given initial capacity.
// The capacity is a hint for pre-allocation; the buffer grows as needed.
func NewBuffer(capacity int) *Buffer {
	return &Buffer{
		buf: make([]byte, 0, capacity),
	}
}

// WriteField appends a length-prefixed field to the buffer.
// Format: [4-byte big-endian length][field bytes]
func (b *Buffer) WriteField(data []byte) *Buffer {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	b.buf = append(b.buf, lenBuf...)
	b.buf = append(b.buf, data...)
	return b
}

// WriteString appends a length-prefixed string field to the buffer.
func (b *Buffer) WriteString(s string) *Buffer {
	return b.WriteField([]byte(s))
}

// WriteBool appends a single byte representing a boolean value.
// true = 0x01, false = 0x00. Fixed-size, no length prefix needed.
func (b *Buffer) WriteBool(v bool) *Buffer {
	if v {
		b.buf = append(b.buf, 1)
	} else {
		b.buf = append(b.buf, 0)
	}
	return b
}

// WriteStrings appends multiple string fields, each individually length-prefixed.
// The count of strings is written first as a 4-byte big-endian uint32 to
// distinguish between different numbers of empty strings.
func (b *Buffer) WriteStrings(ss []string) *Buffer {
	countBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(countBuf, uint32(len(ss)))
	b.buf = append(b.buf, countBuf...)
	for _, s := range ss {
		b.WriteString(s)
	}
	return b
}

// WriteTimeBinary appends time bytes (from time.Time.MarshalBinary) as a length-prefixed field.
// The caller passes the result of time.Time.MarshalBinary() to keep this package
// free of time package dependency.
func (b *Buffer) WriteTimeBinary(timeBytes []byte) *Buffer {
	return b.WriteField(timeBytes)
}

// Bytes returns the accumulated canonical byte sequence.
// The returned slice is a copy; modifying it does not affect the buffer.
func (b *Buffer) Bytes() []byte {
	result := make([]byte, len(b.buf))
	copy(result, b.buf)
	return result
}

// Len returns the current length of the buffer in bytes.
func (b *Buffer) Len() int {
	return len(b.buf)
}

// Reset clears the buffer for reuse, retaining allocated memory.
func (b *Buffer) Reset() {
	b.buf = b.buf[:0]
}

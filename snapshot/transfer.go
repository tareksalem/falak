package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"go.uber.org/zap"
)

const (
	// TransferProtocol is the libp2p stream protocol for moving
	// snapshot files between nodes.
	TransferProtocol = protocol.ID("/falak/snapshot/transfer/1.0")
)

// TransferRequest is sent by the requesting node at the start of a
// transfer stream.
type TransferRequest struct {
	CapsuleID string `json:"capsule_id"`
	Tag       string `json:"tag"`
}

// TransferHeader is sent by the sender before the compressed data stream.
// It contains metadata the receiver needs to verify integrity.
type TransferHeader struct {
	CapsuleID  string `json:"capsule_id"`
	Tag        string `json:"tag"`
	Checksum   string `json:"checksum"`    // SHA-256 of the uncompressed tar-like stream
	TotalSize  int64  `json:"total_size"`  // uncompressed bytes
	FileCount  int    `json:"file_count"`
}

// FileHeader precedes each file in the transfer stream. Files are sent
// as a sequence of (FileHeader, bytes) pairs inside the zstd-compressed
// stream.
type FileHeader struct {
	Name string `json:"name"` // relative path within the snapshot directory
	Size int64  `json:"size"`
}

// TransferServer handles incoming snapshot pull requests. It reads the
// local snapshot directory, streams the files through zstd, and sends
// them over a libp2p stream.
type TransferServer struct {
	host   host.Host
	store  *Store
	logger *zap.Logger

	mu       sync.RWMutex
	sending  map[string]bool // snapshotPath → currently sending (read lock)
}

// TransferServerOption configures a TransferServer.
type TransferServerOption func(*TransferServer)

// WithTransferServerLogger sets the logger.
func WithTransferServerLogger(logger *zap.Logger) TransferServerOption {
	return func(s *TransferServer) { s.logger = logger }
}

// NewTransferServer creates and registers the transfer stream handler.
func NewTransferServer(h host.Host, store *Store, opts ...TransferServerOption) *TransferServer {
	s := &TransferServer{
		host:    h,
		store:   store,
		logger:  zap.NewNop(),
		sending: make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	h.SetStreamHandler(TransferProtocol, s.handleStream)
	return s
}

// Stop removes the stream handler.
func (s *TransferServer) Stop() {
	s.host.RemoveStreamHandler(TransferProtocol)
}

// handleStream is the libp2p stream handler for incoming transfer
// requests.
func (s *TransferServer) handleStream(stream network.Stream) {
	defer stream.Close()
	// Set a generous deadline for the entire transfer. Large snapshots
	// may take minutes. 10 minutes is the upper bound; real transfers
	// finish much sooner. Without this, a slow/malicious peer parks
	// a goroutine indefinitely.
	stream.SetDeadline(time.Now().Add(10 * time.Minute))

	var req TransferRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		s.logger.Debug("transfer: decode request failed", zap.Error(err))
		return
	}

	rec, err := s.store.Get(req.CapsuleID, req.Tag)
	if err != nil || rec == nil {
		s.logger.Debug("transfer: snapshot not found",
			zap.String("capsule_id", req.CapsuleID),
			zap.String("tag", req.Tag))
		// Send an empty header with file_count=0 to signal "not found".
		json.NewEncoder(stream).Encode(TransferHeader{FileCount: 0})
		return
	}

	// Hold a read lock on this snapshot to prevent eviction mid-send.
	snapPath := s.store.SnapshotPath(req.CapsuleID, req.Tag)
	s.mu.Lock()
	s.sending[snapPath] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sending, snapPath)
		s.mu.Unlock()
	}()

	// Collect files and compute checksum.
	files, totalSize, checksum, err := collectFiles(snapPath)
	if err != nil {
		s.logger.Warn("transfer: collect files failed",
			zap.String("path", snapPath), zap.Error(err))
		json.NewEncoder(stream).Encode(TransferHeader{FileCount: 0})
		return
	}

	// Send header.
	hdr := TransferHeader{
		CapsuleID: req.CapsuleID,
		Tag:       req.Tag,
		Checksum:  checksum,
		TotalSize: totalSize,
		FileCount: len(files),
	}
	if err := json.NewEncoder(stream).Encode(hdr); err != nil {
		s.logger.Warn("transfer: send header failed", zap.Error(err))
		return
	}

	// Stream files through zstd.
	zw, err := zstd.NewWriter(stream)
	if err != nil {
		s.logger.Warn("transfer: zstd writer failed", zap.Error(err))
		return
	}

	for _, f := range files {
		if err := sendFile(zw, snapPath, f); err != nil {
			s.logger.Warn("transfer: send file failed",
				zap.String("file", f.Name), zap.Error(err))
			zw.Close()
			return
		}
	}
	zw.Close()

	s.logger.Info("transfer: sent snapshot",
		zap.String("capsule_id", req.CapsuleID),
		zap.String("tag", req.Tag),
		zap.Int("files", len(files)),
		zap.Int64("bytes", totalSize))
}

// PullSnapshot requests a snapshot from a remote peer and writes it to
// the local store. Returns the record metadata on success.
func PullSnapshot(
	ctx context.Context,
	h host.Host,
	peerID peer.ID,
	store *Store,
	capsuleID, tag string,
	logger *zap.Logger,
) (*Record, error) {
	stream, err := h.NewStream(ctx, peerID, TransferProtocol)
	if err != nil {
		return nil, fmt.Errorf("transfer pull: open stream: %w", err)
	}
	defer stream.Close()

	// Send request.
	req := TransferRequest{CapsuleID: capsuleID, Tag: tag}
	if err := json.NewEncoder(stream).Encode(req); err != nil {
		return nil, fmt.Errorf("transfer pull: encode request: %w", err)
	}

	// Read header.
	var hdr TransferHeader
	if err := json.NewDecoder(stream).Decode(&hdr); err != nil {
		return nil, fmt.Errorf("transfer pull: decode header: %w", err)
	}
	if hdr.FileCount == 0 {
		return nil, fmt.Errorf("transfer pull: peer does not have snapshot %s/%s", capsuleID, tag)
	}

	// Prepare local directory.
	if err := store.EnsureDir(capsuleID, tag); err != nil {
		return nil, fmt.Errorf("transfer pull: ensure dir: %w", err)
	}
	destPath := store.SnapshotPath(capsuleID, tag)

	// Read compressed stream and verify checksum.
	zr, err := zstd.NewReader(stream)
	if err != nil {
		store.RemoveDir(capsuleID, tag)
		return nil, fmt.Errorf("transfer pull: zstd reader: %w", err)
	}
	defer zr.Close()

	hasher := sha256.New()
	var totalReceived int64

	for i := 0; i < hdr.FileCount; i++ {
		n, err := receiveFile(zr, destPath, hasher)
		if err != nil {
			store.RemoveDir(capsuleID, tag)
			return nil, fmt.Errorf("transfer pull: receive file %d/%d: %w", i+1, hdr.FileCount, err)
		}
		totalReceived += n
	}

	// Verify checksum.
	gotChecksum := hex.EncodeToString(hasher.Sum(nil))
	if gotChecksum != hdr.Checksum {
		store.RemoveDir(capsuleID, tag)
		return nil, fmt.Errorf("transfer pull: checksum mismatch: got %s, want %s", gotChecksum, hdr.Checksum)
	}

	// Record in store.
	now := time.Now()
	rec := Record{
		CapsuleID:    capsuleID,
		Tag:          tag,
		Size:         totalReceived,
		Path:         destPath,
		Checksum:     gotChecksum,
		CreatedAt:    now,
		LastAccessed: now,
		TTL:          72 * time.Hour, // default 72h, caller should override from capsule config
	}
	if err := store.Put(rec); err != nil {
		return nil, fmt.Errorf("transfer pull: store put: %w", err)
	}

	logger.Info("transfer: received snapshot",
		zap.String("capsule_id", capsuleID),
		zap.String("tag", tag),
		zap.Int64("bytes", totalReceived),
		zap.String("checksum", gotChecksum))

	return &rec, nil
}

// --- File I/O helpers ----------------------------------------------------

// fileEntry is a file to be transferred.
type fileEntry struct {
	Name string // relative to snapshot dir
	Size int64
}

// collectFiles walks the snapshot directory, collecting files and
// computing a SHA-256 checksum of all file contents concatenated.
func collectFiles(snapPath string) ([]fileEntry, int64, string, error) {
	var files []fileEntry
	var totalSize int64
	hasher := sha256.New()

	err := filepath.Walk(snapPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapPath, path)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{Name: rel, Size: info.Size()})
		totalSize += info.Size()

		// Hash the file contents for checksum.
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(hasher, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, "", err
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	return files, totalSize, checksum, nil
}

// sendFile writes a single file (header + content) to the zstd writer.
// Format: 4-byte header length (big-endian) + JSON header + file bytes.
func sendFile(w io.Writer, snapPath string, f fileEntry) error {
	hdr := FileHeader{Name: f.Name, Size: f.Size}
	hdrBytes, err := json.Marshal(hdr)
	if err != nil {
		return err
	}

	// Write header length + header.
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(hdrBytes)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := w.Write(hdrBytes); err != nil {
		return err
	}

	// Write file content.
	path := filepath.Join(snapPath, f.Name)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := io.CopyN(w, file, f.Size); err != nil {
		return err
	}
	return nil
}

// receiveFile reads a single file (header + content) from the zstd reader,
// writes it to disk, and feeds the raw bytes into the hasher for checksum
// verification.
func receiveFile(r io.Reader, destPath string, hasher io.Writer) (int64, error) {
	// Read header length.
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, fmt.Errorf("read header length: %w", err)
	}
	hdrLen := binary.BigEndian.Uint32(lenBuf[:])

	// Read header.
	hdrBytes := make([]byte, hdrLen)
	if _, err := io.ReadFull(r, hdrBytes); err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	var hdr FileHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return 0, fmt.Errorf("unmarshal header: %w", err)
	}

	// Validate the file name to prevent path traversal attacks.
	// A malicious sender could set hdr.Name to "../../etc/cron.d/root"
	// and write outside the snapshot directory.
	cleanName := filepath.Clean(hdr.Name)
	if cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanName) {
		return 0, fmt.Errorf("rejected path traversal in file name: %q", hdr.Name)
	}

	// Create the file on disk.
	filePath := filepath.Join(destPath, cleanName)
	if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
		return 0, fmt.Errorf("mkdir for %s: %w", cleanName, err)
	}
	f, err := os.Create(filePath)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", hdr.Name, err)
	}
	defer f.Close()

	// Read file content, writing to both disk and hasher.
	mw := io.MultiWriter(f, hasher)
	n, err := io.CopyN(mw, r, hdr.Size)
	if err != nil {
		return 0, fmt.Errorf("read content for %s: %w", hdr.Name, err)
	}
	return n, nil
}

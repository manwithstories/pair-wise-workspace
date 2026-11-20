// Package index persists the keyframe index in fixed-packet-count chunks.
// Each chunk carries a format version, a SHA-256 checksum and a completion
// marker. Writes are atomic (tmp file + rename), so a crash leaves either
// the previous complete chunk or a clearly-marked incomplete tail that the
// restart logic can rebuild. Memory stays bounded by one chunk.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"archive/internal/model"
)

// Version is the on-disk index format version.
const Version = 1

// KeyframeEntry is one indexed keyframe pointer.
type KeyframeEntry struct {
	SSRC       uint32 `json:"ssrc"`
	Sequence   int64  `json:"sequence"`
	TimeNanos  int64  `json:"time_nanos"`
	FrameIndex int64  `json:"frame_index"`
	Segment    int    `json:"segment"`
}

// Chunk is one fixed-size, checksummed block of the index.
type Chunk struct {
	Index       int             `json:"index"`
	Version     int             `json:"version"`
	Complete    bool            `json:"complete"`
	PacketStart int64           `json:"packet_start"`
	PacketEnd   int64           `json:"packet_end"`
	Entries     []KeyframeEntry `json:"entries"`
	Checksum    string          `json:"checksum"` // SHA-256 over the canonical entry payload
}

// Store appends completed chunks to a manifest directory. The manifest
// binds the index to the input and parameter digests so a restart can
// refuse to resume a mismatched job.
type Store struct {
	dir          string
	chunkPackets int
	chunks       []Chunk
	pending      []KeyframeEntry
	pendingStart int64
	nextPacket   int64
}

// Manifest is the persisted job identity and chunk listing.
type Manifest struct {
	Version      int     `json:"version"`
	InputDigest  string  `json:"input_digest"`
	ParamDigest  string  `json:"param_digest"`
	ChunkPackets int     `json:"chunk_packets"`
	Chunks       []Chunk `json:"chunks"`
}

// NewStore opens (or creates) a chunked index in dir.
func NewStore(dir string, chunkPackets int) (*Store, error) {
	if chunkPackets <= 0 {
		return nil, model.Newf(model.KindParam, "chunk-packets", "must be > 0, got %d", chunkPackets)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.New(model.KindIO, dir, "cannot create index dir", err)
	}
	return &Store{dir: dir, chunkPackets: chunkPackets}, nil
}

// Add folds one keyframe entry into the pending chunk and flushes the chunk
// once it spans chunkPackets source packets.
func (s *Store) Add(e KeyframeEntry, packetOrdinal int64) error {
	if len(s.pending) == 0 {
		s.pendingStart = packetOrdinal
	}
	s.pending = append(s.pending, e)
	s.nextPacket = packetOrdinal + 1
	if s.nextPacket-s.pendingStart >= int64(s.chunkPackets) {
		return s.flushChunk(true)
	}
	return nil
}

// FlushTail writes any remaining entries as an explicitly incomplete tail
// chunk, which a later restart is allowed to rebuild.
func (s *Store) FlushTail() error {
	if len(s.pending) == 0 {
		return nil
	}
	return s.flushChunk(false)
}

func (s *Store) flushChunk(complete bool) error {
	c := Chunk{
		Index:       len(s.chunks),
		Version:     Version,
		Complete:    complete,
		PacketStart: s.pendingStart,
		PacketEnd:   s.nextPacket - 1,
		Entries:     append([]KeyframeEntry(nil), s.pending...),
	}
	c.Checksum = checksum(c)
	if err := s.writeChunkFile(c); err != nil {
		return err
	}
	s.chunks = append(s.chunks, c)
	s.pending = nil
	return nil
}

func (s *Store) writeChunkFile(c Chunk) error {
	path := filepath.Join(s.dir, chunkName(c.Index))
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return model.New(model.KindIndex, path, "cannot encode chunk", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.New(model.KindIO, path, "cannot write chunk", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.New(model.KindIO, path, "cannot commit chunk", err)
	}
	return nil
}

// SaveManifest writes the job manifest atomically.
func (s *Store) SaveManifest(inputDigest, paramDigest string) error {
	m := Manifest{
		Version: Version, InputDigest: inputDigest, ParamDigest: paramDigest,
		ChunkPackets: s.chunkPackets, Chunks: s.chunks,
	}
	path := filepath.Join(s.dir, "manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return model.New(model.KindIndex, path, "cannot encode manifest", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.New(model.KindIO, path, "cannot write manifest", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.New(model.KindIO, path, "cannot commit manifest", err)
	}
	return nil
}

// checksum hashes the canonical entry payload (excluding the checksum
// field itself) so corruption of any entry is detectable.
func checksum(c Chunk) string {
	h := sha256.New()
	write := func(b []byte) { h.Write(b) }
	var buf [8]byte
	put := func(v int64) {
		for i := 7; i >= 0; i-- {
			buf[i] = byte(v)
			v >>= 8
		}
		write(buf[:])
	}
	put(int64(c.Index))
	put(c.PacketStart)
	put(c.PacketEnd)
	for _, e := range c.Entries {
		put(int64(e.SSRC))
		put(e.Sequence)
		put(e.TimeNanos)
		put(e.FrameIndex)
		put(int64(e.Segment))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func chunkName(i int) string {
	return "chunk_" + hex.EncodeToString([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}) + ".json"
}

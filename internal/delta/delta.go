package delta

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"example.com/gkpack/internal/manifest"
	"example.com/gkpack/internal/pack"
	"example.com/gkpack/internal/zstdpool"
)

const (
	patchMagic        = "GKPATCH1"
	patchFormat       = "gkpack-patch-v1"
	maxPatchHeaderLen = 512 << 20
)

// Direction is one independently applicable side of a bidirectional patch.
type Direction struct {
	Manifest    manifest.Manifest  `json:"manifest"`
	Dict        []byte             `json:"dict,omitempty"`
	DictMode    zstdpool.DictMode  `json:"dict_mode,omitempty"`
	DictID      uint32             `json:"dict_id,omitempty"`
	Chunks      []pack.ChunkRecord `json:"chunks"`
	PayloadSize uint64             `json:"payload_size"`
}

// Header is sealed with a SHA-256 in the patch container.
type Header struct {
	Format        string    `json:"format"`
	BaseRoot      string    `json:"base_root"`
	TargetRoot    string    `json:"target_root"`
	BaseVersion   string    `json:"base_version"`
	TargetVersion string    `json:"target_version"`
	Forward       Direction `json:"forward"`
	Reverse       Direction `json:"reverse"`
}

// CreateOptions selects the two archives and patch output.
type CreateOptions struct {
	BaseArchive   string
	TargetArchive string
	PatchPath     string
}

// CreateResult reports patch statistics from the forward direction.
type CreateResult struct {
	BaseVersion   string
	TargetVersion string
	BaseRoot      string
	TargetRoot    string
	ChangedBlocks int
	TargetBlocks  int
	ReusePercent  float64
	PatchBytes    int64
	NewRawBytes   uint64
	PatchRatio    float64
	AddedFiles    int
	RemovedFiles  int
	ForwardChunks int
	ReverseChunks int
}

// Create builds a single patch that can upgrade BaseArchive to TargetArchive
// and roll TargetArchive back to BaseArchive.
func Create(opts CreateOptions) (CreateResult, error) {
	if opts.BaseArchive == "" || opts.TargetArchive == "" || opts.PatchPath == "" {
		return CreateResult{}, errors.New("base archive, target archive, and patch path are required")
	}
	base, err := pack.Open(opts.BaseArchive)
	if err != nil {
		return CreateResult{}, fmt.Errorf("open base archive: %w", err)
	}
	defer base.Close()
	target, err := pack.Open(opts.TargetArchive)
	if err != nil {
		return CreateResult{}, fmt.Errorf("open target archive: %w", err)
	}
	defer target.Close()

	baseManifest := base.Manifest()
	targetManifest := target.Manifest()
	baseSet := baseManifest.BlockSet()
	targetSet := targetManifest.BlockSet()

	forward := Direction{
		Manifest: targetManifest,
		Dict:     target.Header().Dict,
		DictMode: target.Header().DictMode,
		DictID:   target.Header().DictID,
	}
	reverse := Direction{
		Manifest: baseManifest,
		Dict:     base.Header().Dict,
		DictMode: base.Header().DictMode,
		DictID:   base.Header().DictID,
	}
	var forwardRaw uint64
	for _, rec := range target.Chunks() {
		if baseSet[rec.Hash] {
			continue
		}
		rec.Offset = forward.PayloadSize
		forward.PayloadSize += uint64(rec.StoredSize)
		forward.Chunks = append(forward.Chunks, rec)
		forwardRaw += uint64(rec.RawSize)
	}
	for _, rec := range base.Chunks() {
		if targetSet[rec.Hash] {
			continue
		}
		rec.Offset = reverse.PayloadSize
		reverse.PayloadSize += uint64(rec.StoredSize)
		reverse.Chunks = append(reverse.Chunks, rec)
	}
	header := Header{
		Format:        patchFormat,
		BaseRoot:      baseManifest.Root,
		TargetRoot:    targetManifest.Root,
		BaseVersion:   base.Header().Version,
		TargetVersion: target.Header().Version,
		Forward:       forward,
		Reverse:       reverse,
	}
	if err := writePatch(opts.PatchPath, header, base, target); err != nil {
		return CreateResult{}, err
	}
	info, err := os.Stat(opts.PatchPath)
	if err != nil {
		return CreateResult{}, err
	}
	targetBlocks := len(targetSet)
	changed := len(forward.Chunks)
	reuse := 100.0
	if targetBlocks > 0 {
		reuse = float64(targetBlocks-changed) * 100 / float64(targetBlocks)
	}
	ratio := 0.0
	if info.Size() > 0 {
		ratio = float64(forwardRaw) / float64(info.Size())
	}
	added, removed := fileDiff(baseManifest, targetManifest)
	return CreateResult{
		BaseVersion:   base.Header().Version,
		TargetVersion: target.Header().Version,
		BaseRoot:      baseManifest.Root,
		TargetRoot:    targetManifest.Root,
		ChangedBlocks: changed,
		TargetBlocks:  targetBlocks,
		ReusePercent:  reuse,
		PatchBytes:    info.Size(),
		NewRawBytes:   forwardRaw,
		PatchRatio:    ratio,
		AddedFiles:    added,
		RemovedFiles:  removed,
		ForwardChunks: len(forward.Chunks),
		ReverseChunks: len(reverse.Chunks),
	}, nil
}

func fileDiff(base, target manifest.Manifest) (added, removed int) {
	basePaths := make(map[string]bool, len(base.Files))
	for _, f := range base.Files {
		basePaths[f.Path] = true
	}
	targetPaths := make(map[string]bool, len(target.Files))
	for _, f := range target.Files {
		targetPaths[f.Path] = true
	}
	for p := range targetPaths {
		if !basePaths[p] {
			added++
		}
	}
	for p := range basePaths {
		if !targetPaths[p] {
			removed++
		}
	}
	return added, removed
}

func writePatch(path string, header Header, base, target *pack.Reader) error {
	header.Forward.Chunks = sortedChunks(header.Forward.Chunks)
	header.Reverse.Chunks = sortedChunks(header.Reverse.Chunks)
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(headerJSON) > maxPatchHeaderLen {
		return errors.New("patch header too large")
	}
	sum := sha256.Sum256(headerJSON)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".gkpack-patch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	w := bufio.NewWriterSize(tmp, 256*1024)
	if _, err := w.WriteString(patchMagic); err != nil {
		return err
	}
	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(len(headerJSON)))
	if _, err := w.Write(sizeBuf[:]); err != nil {
		return err
	}
	if _, err := w.Write(sum[:]); err != nil {
		return err
	}
	if _, err := w.Write(headerJSON); err != nil {
		return err
	}
	if err := copyDirection(w, target, header.Forward.Chunks); err != nil {
		return fmt.Errorf("forward payload: %w", err)
	}
	if err := copyDirection(w, base, header.Reverse.Chunks); err != nil {
		return fmt.Errorf("reverse payload: %w", err)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func sortedChunks(chunks []pack.ChunkRecord) []pack.ChunkRecord {
	out := append([]pack.ChunkRecord(nil), chunks...)
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

func copyDirection(w io.Writer, reader *pack.Reader, chunks []pack.ChunkRecord) error {
	for _, rec := range chunks {
		source, ok := reader.Chunk(rec.Hash)
		if !ok {
			return fmt.Errorf("source block %s is missing", rec.Hash)
		}
		stored, err := reader.ReadStored(source)
		if err != nil {
			return err
		}
		if len(stored) != int(rec.StoredSize) {
			return fmt.Errorf("block %s: stored size changed", rec.Hash)
		}
		if _, err := w.Write(stored); err != nil {
			return err
		}
	}
	return nil
}

// Reader provides independent random access to both patch directions.
type Reader struct {
	file         *os.File
	fileSize     int64
	header       Header
	headerSize   int64
	forwardStart int64
	reverseStart int64
	forwardMap   map[string]pack.ChunkRecord
	reverseMap   map[string]pack.ChunkRecord
	forwardPool  *zstdpool.Pool
	reversePool  *zstdpool.Pool
}

// Open validates the patch container and both manifests.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{file: f}
	if err := r.readHeader(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func (r *Reader) readHeader() error {
	info, err := r.file.Stat()
	if err != nil {
		return err
	}
	r.fileSize = info.Size()
	prefix := make([]byte, 8+8+sha256.Size)
	if _, err := io.ReadFull(r.file, prefix); err != nil {
		return fmt.Errorf("patch truncated header: %w", err)
	}
	if string(prefix[:8]) != patchMagic {
		return errors.New("patch magic mismatch")
	}
	headerLen := binary.BigEndian.Uint64(prefix[8:16])
	if headerLen == 0 || headerLen > maxPatchHeaderLen {
		return fmt.Errorf("patch header length %d invalid", headerLen)
	}
	if int64(48+headerLen) > r.fileSize {
		return errors.New("patch truncated: header extends past end of file")
	}
	headerJSON := make([]byte, int(headerLen))
	if _, err := io.ReadFull(r.file, headerJSON); err != nil {
		return err
	}
	sum := sha256.Sum256(headerJSON)
	if !bytesEqual(sum[:], prefix[16:]) {
		return errors.New("patch manifest/header checksum mismatch")
	}
	if err := json.Unmarshal(headerJSON, &r.header); err != nil {
		return fmt.Errorf("decode patch header: %w", err)
	}
	if r.header.Format != patchFormat {
		return fmt.Errorf("unsupported patch format %q", r.header.Format)
	}
	if err := r.header.Forward.Manifest.Validate(); err != nil {
		return fmt.Errorf("forward manifest invalid: %w", err)
	}
	if err := r.header.Reverse.Manifest.Validate(); err != nil {
		return fmt.Errorf("reverse manifest invalid: %w", err)
	}
	if r.header.Forward.Manifest.Root != r.header.TargetRoot {
		return errors.New("forward manifest root does not match target root")
	}
	if r.header.Reverse.Manifest.Root != r.header.BaseRoot {
		return errors.New("reverse manifest root does not match base root")
	}
	r.headerSize = int64(headerLen)
	r.forwardStart = 48 + r.headerSize
	r.reverseStart = r.forwardStart + int64(r.header.Forward.PayloadSize)
	end := r.reverseStart + int64(r.header.Reverse.PayloadSize)
	if end > r.fileSize {
		return fmt.Errorf("patch truncated: payload needs %d bytes, file has %d", end, r.fileSize)
	}
	if end != r.fileSize {
		return fmt.Errorf("patch has %d trailing bytes", r.fileSize-end)
	}
	r.forwardMap = make(map[string]pack.ChunkRecord, len(r.header.Forward.Chunks))
	r.reverseMap = make(map[string]pack.ChunkRecord, len(r.header.Reverse.Chunks))
	if err := validateDirection(r.header.Forward, r.forwardMap); err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	if err := validateDirection(r.header.Reverse, r.reverseMap); err != nil {
		return fmt.Errorf("reverse: %w", err)
	}
	fp, err := zstdpool.New(zstdpool.Dict{Data: r.header.Forward.Dict, Mode: r.header.Forward.DictMode, ID: r.header.Forward.DictID})
	if err != nil {
		return fmt.Errorf("forward dictionary: %w", err)
	}
	rp, err := zstdpool.New(zstdpool.Dict{Data: r.header.Reverse.Dict, Mode: r.header.Reverse.DictMode, ID: r.header.Reverse.DictID})
	if err != nil {
		return fmt.Errorf("reverse dictionary: %w", err)
	}
	r.forwardPool = fp
	r.reversePool = rp
	return nil
}

func validateDirection(dir Direction, byHash map[string]pack.ChunkRecord) error {
	var offset uint64
	for i, rec := range dir.Chunks {
		if rec.Hash == "" || len(rec.Hash) != sha256.Size*2 {
			return fmt.Errorf("chunk %d has invalid hash", i)
		}
		if i > 0 && dir.Chunks[i-1].Hash >= rec.Hash {
			return fmt.Errorf("chunks not strictly sorted at %s", rec.Hash)
		}
		if rec.Offset != offset {
			return fmt.Errorf("chunk %s offset %d, want %d", rec.Hash, rec.Offset, offset)
		}
		if rec.Method != "zstd" && rec.Method != "raw" {
			return fmt.Errorf("chunk %s method invalid", rec.Hash)
		}
		offset += uint64(rec.StoredSize)
		byHash[rec.Hash] = rec
	}
	if offset != dir.PayloadSize {
		return fmt.Errorf("payload size %d, chunks total %d", dir.PayloadSize, offset)
	}
	return nil
}

// Close closes the patch file.
func (r *Reader) Close() error { return r.file.Close() }

// Header returns patch metadata.
func (r *Reader) Header() Header { return r.header }

// ManifestFor returns the target manifest for a direction.
func (r *Reader) ManifestFor(reverse bool) manifest.Manifest {
	if reverse {
		return r.header.Reverse.Manifest
	}
	return r.header.Forward.Manifest
}

// HasChunk reports whether a direction carries the given chunk.
func (r *Reader) HasChunk(reverse bool, hash string) bool {
	if reverse {
		_, ok := r.reverseMap[hash]
		return ok
	}
	_, ok := r.forwardMap[hash]
	return ok
}

// ReadNewChunk reads and verifies a chunk carried by the selected direction.
func (r *Reader) ReadNewChunk(reverse bool, hash string) ([]byte, error) {
	dir := r.header.Forward
	start := r.forwardStart
	pool := r.forwardPool
	index := r.forwardMap
	if reverse {
		dir = r.header.Reverse
		start = r.reverseStart
		pool = r.reversePool
		index = r.reverseMap
	}
	rec, ok := index[hash]
	if !ok {
		return nil, fmt.Errorf("block %s is not in patch", hash)
	}
	if rec.Offset > dir.PayloadSize || uint64(rec.StoredSize) > dir.PayloadSize-rec.Offset {
		return nil, fmt.Errorf("patch block %s truncated", hash)
	}
	stored := make([]byte, rec.StoredSize)
	if _, err := r.file.ReadAt(stored, start+int64(rec.Offset)); err != nil {
		return nil, fmt.Errorf("read patch block %s: %w", hash, err)
	}
	var raw []byte
	var err error
	switch rec.Method {
	case "raw":
		raw = stored
	case "zstd":
		raw, err = pool.Decompress(stored, int(rec.RawSize))
		if err != nil {
			return nil, fmt.Errorf("patch block %s: %w", hash, err)
		}
	default:
		return nil, fmt.Errorf("patch block %s: unknown method", hash)
	}
	if len(raw) != int(rec.RawSize) {
		return nil, fmt.Errorf("patch block %s raw size %d, want %d", hash, len(raw), rec.RawSize)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("patch block %s hash mismatch", hash)
	}
	return raw, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

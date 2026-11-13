package pack

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"example.com/gkpack/internal/manifest"
	"example.com/gkpack/internal/store"
	"example.com/gkpack/internal/zstdpool"
)

const (
	archiveMagic        = "GKPK0001"
	archiveFormat       = "gkpack-archive-v1"
	maxArchiveHeaderLen = 256 << 20
)

// ChunkRecord is the random-access index for one stored block.
type ChunkRecord struct {
	Hash       string `json:"hash"`
	RawSize    uint32 `json:"raw_size"`
	StoredSize uint32 `json:"stored_size"`
	Method     string `json:"method"` // zstd or raw
	Offset     uint64 `json:"offset"` // relative to payload start
}

// ArchiveHeader is sealed by the SHA-256 immediately following the length field.
type ArchiveHeader struct {
	Format     string            `json:"format"`
	Version    string            `json:"version"`
	Manifest   manifest.Manifest `json:"manifest"`
	Dict       []byte            `json:"dict,omitempty"`
	DictMode   zstdpool.DictMode `json:"dict_mode,omitempty"`
	DictID     uint32            `json:"dict_id,omitempty"`
	Chunks     []ChunkRecord     `json:"chunks"`
	ChunkCount int               `json:"chunk_count"`
}

// BuildOptions controls a deterministic archive build.
type BuildOptions struct {
	SourceDir   string
	ArchivePath string
	Version     string
	Workers     int
}

// BuildResult reports the archive size and content statistics.
type BuildResult struct {
	Version       string
	Root          string
	RawBytes      uint64
	PackedBytes   int64
	Files         int
	Blocks        int
	UniqueBlocks  int
	Compressed    int
	DictionaryLen int
}

// Build scans SourceDir, trains a version dictionary, compresses unique blocks
// concurrently, and writes one deterministic archive. Archive order is always
// hash order, independent of Worker count.
func Build(opts BuildOptions) (BuildResult, error) {
	if opts.SourceDir == "" || opts.ArchivePath == "" {
		return BuildResult{}, errors.New("source and archive paths are required")
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > 32 {
		workers = 32
	}
	if workers < 1 {
		workers = 1
	}

	archiveDir := filepath.Dir(opts.ArchivePath)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return BuildResult{}, err
	}
	st, cleanup, err := store.NewTemp(archiveDir)
	if err != nil {
		return BuildResult{}, err
	}
	defer cleanup()

	m, scan, err := ScanSource(opts.SourceDir, st)
	if err != nil {
		return BuildResult{}, err
	}
	blocks := st.Entries()

	dict, err := sampleDictionary(st, blocks)
	if err != nil {
		return BuildResult{}, err
	}
	pool, err := zstdpool.New(dict)
	if err != nil {
		return BuildResult{}, fmt.Errorf("create zstd pool: %w", err)
	}

	compDir, err := os.MkdirTemp(archiveDir, ".gkpack-compress-*")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(compDir)

	type result struct {
		record ChunkRecord
		path   string
		err    error
	}
	results := make([]result, len(blocks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				block := blocks[idx]
				raw, err := st.Get(block.Hash)
				if err != nil {
					results[idx].err = err
					setFirstError(&firstErr, &errMu, err)
					continue
				}
				stored, err := pool.Compress(raw)
				if err != nil {
					results[idx].err = err
					setFirstError(&firstErr, &errMu, err)
					continue
				}
				method := "zstd"
				if len(stored) >= len(raw) {
					stored = raw
					method = "raw"
				}
				path := filepath.Join(compDir, block.Hash)
				if err := os.WriteFile(path, stored, 0o600); err != nil {
					results[idx].err = err
					setFirstError(&firstErr, &errMu, err)
					continue
				}
				results[idx] = result{
					record: ChunkRecord{
						Hash:       block.Hash,
						RawSize:    uint32(len(raw)),
						StoredSize: uint32(len(stored)),
						Method:     method,
					},
					path: path,
				}
			}
		}()
	}
	for i := range blocks {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return BuildResult{}, firstErr
	}

	header := ArchiveHeader{
		Format:     archiveFormat,
		Version:    opts.Version,
		Manifest:   m,
		Dict:       dict.Data,
		DictMode:   dict.Mode,
		DictID:     dict.ID,
		Chunks:     make([]ChunkRecord, 0, len(blocks)),
		ChunkCount: len(blocks),
	}
	var offset uint64
	compressedCount := 0
	for _, result := range results {
		rec := result.record
		rec.Offset = offset
		offset += uint64(rec.StoredSize)
		if rec.Method == "zstd" {
			compressedCount++
		}
		header.Chunks = append(header.Chunks, rec)
	}
	if err := writeArchive(opts.ArchivePath, header, compDir); err != nil {
		return BuildResult{}, err
	}
	info, err := os.Stat(opts.ArchivePath)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		Version:       opts.Version,
		Root:          m.Root,
		RawBytes:      scan.RawBytes,
		PackedBytes:   info.Size(),
		Files:         scan.Files,
		Blocks:        scan.Chunks,
		UniqueBlocks:  scan.UniqueBlocks,
		Compressed:    compressedCount,
		DictionaryLen: len(dict.Data),
	}, nil
}

func setFirstError(dst *error, mu *sync.Mutex, err error) {
	mu.Lock()
	defer mu.Unlock()
	if *dst == nil {
		*dst = err
	}
}

func sampleDictionary(st *store.Store, blocks []store.Block) (zstdpool.Dict, error) {
	const wanted = 512
	if len(blocks) == 0 {
		return zstdpool.Dict{Mode: zstdpool.DictNone}, nil
	}
	samples := make([][]byte, 0, wanted)
	step := 1
	if len(blocks) > wanted {
		step = len(blocks) / wanted
	}
	var total int
	for i := 0; i < len(blocks) && len(samples) < wanted; i += step {
		block := blocks[i]
		if total+block.Size > zstdpool.MaxTrainSamples {
			break
		}
		data, err := st.Get(block.Hash)
		if err != nil {
			return zstdpool.Dict{}, err
		}
		samples = append(samples, data)
		total += len(data)
	}
	return zstdpool.TrainDict(samples)
}

func writeArchive(path string, header ArchiveHeader, compDir string) error {
	header.Chunks = append([]ChunkRecord(nil), header.Chunks...)
	sort.Slice(header.Chunks, func(i, j int) bool { return header.Chunks[i].Hash < header.Chunks[j].Hash })
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(headerJSON) > maxArchiveHeaderLen {
		return errors.New("archive header too large")
	}
	sum := sha256.Sum256(headerJSON)
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gkpack-archive-*")
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
	if _, err := w.WriteString(archiveMagic); err != nil {
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
	for _, rec := range header.Chunks {
		f, err := os.Open(filepath.Join(compDir, rec.Hash))
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(w, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != int64(rec.StoredSize) {
			return fmt.Errorf("block %s: copied %d bytes, want %d", rec.Hash, n, rec.StoredSize)
		}
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

// Reader provides random access to an archive.
type Reader struct {
	path         string
	file         *os.File
	fileSize     int64
	header       ArchiveHeader
	payloadStart int64
	payloadSize  int64
	byHash       map[string]ChunkRecord
	pool         *zstdpool.Pool
}

// Open opens an archive and validates its sealed header and manifest root.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{path: path, file: f}
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
		return fmt.Errorf("archive truncated header: %w", err)
	}
	if string(prefix[:8]) != archiveMagic {
		return fmt.Errorf("archive magic mismatch")
	}
	headerLen := binary.BigEndian.Uint64(prefix[8:16])
	if headerLen == 0 || headerLen > maxArchiveHeaderLen {
		return fmt.Errorf("archive header length %d invalid", headerLen)
	}
	wantSum := prefix[16:]
	if int64(48+headerLen) > r.fileSize {
		return errors.New("archive truncated: header extends past end of file")
	}
	headerJSON := make([]byte, int(headerLen))
	if _, err := io.ReadFull(r.file, headerJSON); err != nil {
		return fmt.Errorf("archive truncated header: %w", err)
	}
	sum := sha256.Sum256(headerJSON)
	if !equalBytes(sum[:], wantSum) {
		return errors.New("archive manifest/header checksum mismatch")
	}
	if err := json.Unmarshal(headerJSON, &r.header); err != nil {
		return fmt.Errorf("decode archive header: %w", err)
	}
	if r.header.Format != archiveFormat {
		return fmt.Errorf("unsupported archive format %q", r.header.Format)
	}
	if err := r.header.Manifest.Validate(); err != nil {
		return fmt.Errorf("archive manifest invalid: %w", err)
	}
	r.payloadStart = 48 + int64(headerLen)
	r.payloadSize = r.fileSize - r.payloadStart
	r.byHash = make(map[string]ChunkRecord, len(r.header.Chunks))
	var expectOffset uint64
	for i, rec := range r.header.Chunks {
		if rec.Hash == "" || len(rec.Hash) != sha256.Size*2 {
			return fmt.Errorf("archive chunk %d has invalid hash", i)
		}
		if i > 0 && r.header.Chunks[i-1].Hash >= rec.Hash {
			return fmt.Errorf("archive chunks are not strictly sorted at %s", rec.Hash)
		}
		if rec.RawSize == 0 || rec.StoredSize == 0 {
			return fmt.Errorf("archive chunk %s has zero size", rec.Hash)
		}
		if rec.Method != "zstd" && rec.Method != "raw" {
			return fmt.Errorf("archive chunk %s has unknown method %q", rec.Hash, rec.Method)
		}
		if rec.Offset != expectOffset {
			return fmt.Errorf("archive chunk %s has non-contiguous offset", rec.Hash)
		}
		expectOffset += uint64(rec.StoredSize)
		r.byHash[rec.Hash] = rec
	}
	if v, ok := r.header.Manifest.BlockSet()[""]; ok && v {
		return errors.New("archive manifest references an empty block hash")
	}
	dict := zstdpool.Dict{Data: r.header.Dict, Mode: r.header.DictMode, ID: r.header.DictID}
	pool, err := zstdpool.New(dict)
	if err != nil {
		return fmt.Errorf("archive dictionary invalid: %w", err)
	}
	r.pool = pool
	return nil
}

// Close closes the archive.
func (r *Reader) Close() error { return r.file.Close() }

// Header returns a copy of archive metadata.
func (r *Reader) Header() ArchiveHeader { return r.header }

// Manifest returns the archive's canonical manifest.
func (r *Reader) Manifest() manifest.Manifest { return r.header.Manifest }

// Chunks returns the archive chunk index in hash order.
func (r *Reader) Chunks() []ChunkRecord { return append([]ChunkRecord(nil), r.header.Chunks...) }

// PayloadStart and PayloadSize are useful for verification diagnostics.
func (r *Reader) PayloadStart() int64 { return r.payloadStart }
func (r *Reader) PayloadSize() int64  { return r.payloadSize }
func (r *Reader) FileSize() int64     { return r.fileSize }

// Chunk returns the index record for hash.
func (r *Reader) Chunk(hash string) (ChunkRecord, bool) {
	rec, ok := r.byHash[hash]
	return rec, ok
}

// ReadStored returns the exact stored bytes for a record without decoding.
func (r *Reader) ReadStored(rec ChunkRecord) ([]byte, error) {
	if rec.Offset > uint64(r.payloadSize) || uint64(rec.StoredSize) > uint64(r.payloadSize)-rec.Offset {
		return nil, fmt.Errorf("block %s is truncated", rec.Hash)
	}
	data := make([]byte, rec.StoredSize)
	if _, err := r.file.ReadAt(data, r.payloadStart+int64(rec.Offset)); err != nil {
		return nil, fmt.Errorf("read block %s: %w", rec.Hash, err)
	}
	return data, nil
}

// ReadChunk returns a verified raw chunk.
func (r *Reader) ReadChunk(hash string) ([]byte, error) {
	rec, ok := r.byHash[hash]
	if !ok {
		return nil, fmt.Errorf("block %s is missing", hash)
	}
	stored, err := r.ReadStored(rec)
	if err != nil {
		return nil, err
	}
	var raw []byte
	switch rec.Method {
	case "raw":
		raw = stored
	case "zstd":
		raw, err = r.pool.Decompress(stored, int(rec.RawSize))
		if err != nil {
			return nil, fmt.Errorf("block %s: %w", hash, err)
		}
	default:
		return nil, fmt.Errorf("block %s: unknown method %q", hash, rec.Method)
	}
	if len(raw) != int(rec.RawSize) {
		return nil, fmt.Errorf("block %s: raw size %d, want %d", hash, len(raw), rec.RawSize)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("block %s: content hash mismatch", hash)
	}
	return raw, nil
}

// Root returns the full manifest root.
func (r *Reader) Root() string { return r.header.Manifest.Root }

// FilesForBlock maps a block hash to all manifest paths that reference it.
func (r *Reader) FilesForBlock(hash string) []string {
	var out []string
	for _, f := range r.header.Manifest.Files {
		for _, b := range f.Blocks {
			if b.Hash == hash {
				out = append(out, f.Path)
				break
			}
		}
	}
	return out
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// FormatMiB formats a byte count with one decimal place.
func FormatMiB(n uint64) string {
	return fmt.Sprintf("%.1fMiB", float64(n)/(1024*1024))
}

// IsNotExist reports whether err is a missing-file error, kept here so callers
// do not need to know the archive implementation.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

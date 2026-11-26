package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"avpipe/internal/media"
)

const (
	IndexName   = "checkpoint-index.json"
	JournalName = "journal.jsonl"
	ChunkDir    = "chunks"
)

type SegmentRecord struct {
	Seq         int    `json:"seq"`
	ChunkSeq    int    `json:"chunk_seq"`
	Rendition   string `json:"rendition"`
	Bitrate     int64  `json:"bitrate"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	InputOffset int    `json:"input_offset"`
	InputEnd    int    `json:"input_end"`
	ClockEpoch  int    `json:"clock_epoch"`
}

// ChunkRecord is one atomic checkpoint unit (one aligned tactical slot).
type ChunkRecord struct {
	Seq           int                      `json:"seq"`
	ParserVersion string                   `json:"parser_version"`
	SourceHash    string                   `json:"source_hash"`
	ConfigHash    string                   `json:"config_hash"`
	ClockEpochs   map[string][]media.Epoch `json:"clock_epochs"`
	Segments      []SegmentRecord          `json:"segments"`
	Diagnostics   map[string]int           `json:"diagnostics,omitempty"`
}

type Index struct {
	ParserVersion string               `json:"parser_version"`
	SourceHash    string               `json:"source_hash"`
	ConfigHash    string               `json:"config_hash"`
	Chunks        map[int]*ChunkRecord `json:"chunks"`
}

type Store struct {
	Root      string
	IndexPath string
	Journal   string
	ChunksDir string
}

func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "segments"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, ChunkDir), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		Root: root, IndexPath: filepath.Join(root, IndexName),
		Journal: filepath.Join(root, JournalName), ChunksDir: filepath.Join(root, ChunkDir),
	}
	return s, nil
}

func SafePath(root, name string) (string, error) {
	if filepath.IsAbs(name) || strings.Contains(name, "..") {
		return "", media.NewError(media.CodePath, -1, "unsafe relative path %q", name)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(absRoot, name))
	if full != absRoot && !strings.HasPrefix(full, absRoot+string(os.PathSeparator)) {
		return "", media.NewError(media.CodePath, -1, "path %q escapes checkpoint root", name)
	}
	return full, nil
}

// CommitSegment streams content to a temp file while hashing it, fsyncs and
// atomically renames it. No index record exists until this returns.
func (s *Store) CommitSegment(rel string, r io.Reader, base SegmentRecord) (SegmentRecord, error) {
	final, err := SafePath(s.Root, rel)
	if err != nil {
		return base, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return base, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".tmp-*")
	if err != nil {
		return base, media.WrapError(media.CodeIO, -1, err, "create temp segment")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	n, err := io.CopyBuffer(tmp, io.TeeReader(r, h), make([]byte, 32*1024))
	if err != nil {
		tmp.Close()
		return base, media.WrapError(media.CodeIO, -1, err, "hash/write segment")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return base, err
	}
	if err := tmp.Close(); err != nil {
		return base, err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return base, media.WrapError(media.CodeIO, -1, err, "atomic segment rename")
	}
	base.Path = rel
	base.Size = n
	base.SHA256 = hex.EncodeToString(h.Sum(nil))
	return base, nil
}

// CommitChunk persists the immutable chunk, appends the recovery journal and
// then atomically replaces the index. Segment files are already atomic.
func (s *Store) CommitChunk(rec *ChunkRecord) error {
	if rec.ParserVersion == "" {
		rec.ParserVersion = media.ParserVersion
	}
	name, err := SafePath(s.ChunksDir, fmt.Sprintf("chunk-%04d.json", rec.Seq))
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(name, data); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	jdata, _ := json.Marshal(rec)
	if _, err := f.Write(append(jdata, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	idx, err := s.LoadIndex()
	if err != nil {
		return err
	}
	if idx.Chunks == nil {
		idx.Chunks = map[int]*ChunkRecord{}
	}
	idx.ParserVersion = rec.ParserVersion
	idx.SourceHash = rec.SourceHash
	idx.ConfigHash = rec.ConfigHash
	idx.Chunks[rec.Seq] = rec
	out, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.IndexPath, out)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// LoadIndex reads the atomically committed index. A missing index is rebuilt
// from chunk files and the append-only journal.
func (s *Store) LoadIndex() (*Index, error) {
	if b, err := os.ReadFile(s.IndexPath); err == nil {
		var idx Index
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, media.WrapError(media.CodeCheckpoint, -1, err, "parse checkpoint index")
		}
		if idx.Chunks == nil {
			idx.Chunks = map[int]*ChunkRecord{}
		}
		return &idx, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	idx := &Index{Chunks: map[int]*ChunkRecord{}}
	entries, _ := os.ReadDir(s.ChunksDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.ChunksDir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec ChunkRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, err
		}
		idx.Chunks[rec.Seq] = &rec
		idx.SourceHash = rec.SourceHash
		idx.ConfigHash = rec.ConfigHash
		idx.ParserVersion = rec.ParserVersion
	}
	return idx, nil
}

// Verify checks every committed segment against path, size and SHA-256.
func (s *Store) Verify(idx *Index) (verified int, bad []string, err error) {
	seqs := make([]int, 0, len(idx.Chunks))
	for seq := range idx.Chunks {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	for _, seq := range seqs {
		for _, seg := range idx.Chunks[seq].Segments {
			full, err := SafePath(s.Root, seg.Path)
			if err != nil {
				bad = append(bad, seg.Path)
				continue
			}
			b, err := os.ReadFile(full)
			if err != nil {
				bad = append(bad, seg.Path+":missing")
				continue
			}
			sum := sha256.Sum256(b)
			if int64(len(b)) != seg.Size || hex.EncodeToString(sum[:]) != seg.SHA256 {
				bad = append(bad, seg.Path+":hash")
				continue
			}
			verified++
		}
	}
	return verified, bad, nil
}

// CleanOrphans removes temp files and segment artifacts not referenced by the
// committed index. It never follows or accepts an unsafe record path.
func (s *Store) CleanOrphans(idx *Index) ([]string, error) {
	keep := map[string]bool{}
	for _, c := range idx.Chunks {
		for _, seg := range c.Segments {
			full, err := SafePath(s.Root, seg.Path)
			if err != nil {
				continue
			}
			keep[full] = true
		}
	}
	var removed []string
	segRoot := filepath.Join(s.Root, "segments")
	err := filepath.Walk(segRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		if keep[path] {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.Root, path)
		removed = append(removed, rel)
		return nil
	})
	return removed, err
}

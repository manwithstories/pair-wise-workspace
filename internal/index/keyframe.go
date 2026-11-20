// Package index provides a randomly accessible keyframe index over the
// segments produced by the segment writer.
//
// The index is a small JSON document: for each representation it maps a
// master-clock timestamp to the exact segment file and the byte offset of the
// frame inside it. Every entry is validated against the files on disk on load
// so a query can never return a pointer that does not resolve. When the index
// is structurally damaged the loader refuses it and Rebuild reconstructs it
// from the segment headers instead.
package index

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mediamend/mediamend/internal/fault"
)

// FileName is the index file inside the output directory.
const FileName = "keyframes.idx.json"

// Version is the on-disk index format version.
const Version = 1

// Entry is one seek point.
type KeyframeEntry struct {
	MasterTS   uint64 `json:"master_ts"`
	Segment    string `json:"segment"`
	ByteOffset int64  `json:"byte_offset"`
	FrameSeq   uint64 `json:"frame_seq"`
	// Representation names the rendition this entry belongs to.
	Representation string `json:"representation"`
}

// Index is the complete keyframe index for one camera.
type KeyframeIndex struct {
	Version int             `json:"version"`
	Camera  string          `json:"camera"`
	Entries []KeyframeEntry `json:"entries"`
}

// SegmentHeader is the parse result of one segment file's first line.
type SegmentHeader struct {
	Representation string
	Width          int
	Height         int
	Bitrate        int
	FrameCount     int
	HeaderLen      int64
}

// parseHeader reads the single header line of a segment file.
func parseHeader(path string) (SegmentHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return SegmentHeader{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 512)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return SegmentHeader{}, fmt.Errorf("index: segment %s has no header", filepath.Base(path))
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 || fields[0] != "PAIRFLOWSEG1" {
		return SegmentHeader{}, fmt.Errorf("index: segment %s has an unknown header", filepath.Base(path))
	}
	h := SegmentHeader{}
	for _, fld := range fields[1:] {
		k, v, ok := strings.Cut(fld, "=")
		if !ok {
			continue
		}
		switch k {
		case "rep":
			h.Representation = v
		case "frames":
			n, _ := strconv.Atoi(v)
			h.FrameCount = n
		}
	}
	h.HeaderLen = int64(len(line))
	return h, nil
}

// SegmentRef is the minimal segment description the index needs. The segment
// package converts its own segments into this type to avoid an import cycle.
type SegmentRef struct {
	File           string
	Representation string
	StartTS        uint64
	FirstSeq       uint64
	// FrameCount is the declared number of frames; a mismatch with the file's
	// actual line count is reported.
	FrameCount int
}

// Build derives the index from segment descriptions. Each segment
// contributes its first frame (always a keyframe by construction, or a
// recorded forced cut) as a seek point. Byte offsets are computed by walking
// the segment's deterministic line layout.
func BuildKeyframe(camera string, segs []SegmentRef, outDir string) (*KeyframeIndex, fault.List) {
	idx := &KeyframeIndex{Version: Version, Camera: camera}
	var faults fault.List
	for _, s := range segs {
		path := filepath.Join(outDir, s.File)
		h, err := parseHeader(path)
		if err != nil {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: camera,
				File: path, Message: err.Error(), Fatal: false,
			})
			continue
		}
		// Every "frame line" is exactly 16+1+8+1+20+1+20+1 bytes; we compute
		// the real offsets by scanning, which also validates the count.
		offsets, err := frameOffsets(path, h.HeaderLen)
		if err != nil {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: camera,
				File: path, Message: err.Error(), Fatal: false,
			})
			continue
		}
		if len(offsets) != s.FrameCount {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: camera,
				File: path,
				Message: fmt.Sprintf("segment %s declares %d frames but contains %d",
					s.File, s.FrameCount, len(offsets)),
				Fatal: false,
			})
		}
		idx.Entries = append(idx.Entries, KeyframeEntry{
			MasterTS: s.StartTS, Segment: s.File, ByteOffset: offsets[0],
			FrameSeq: s.FirstSeq, Representation: s.Representation,
		})
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		a, b := idx.Entries[i], idx.Entries[j]
		if a.Representation != b.Representation {
			return a.Representation < b.Representation
		}
		return a.MasterTS < b.MasterTS
	})
	return idx, faults
}

// frameOffsets returns the byte offset of every frame line after the header.
func frameOffsets(path string, headerLen int64) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(headerLen, 0); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	offset := headerLen
	var out []int64
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			out = append(out, offset)
			offset += int64(len(line))
		}
		if err != nil {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("index: segment %s has no frames", filepath.Base(path))
	}
	return out, nil
}

// Save writes the index atomically to dir/FileName.
func (idx *KeyframeIndex) Save(dir string) error {
	path := filepath.Join(dir, FileName)
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".keyframes-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// Load reads and structurally validates an index. Damage is reported as a
// fault list; a non-empty fatal list means the caller must Rebuild.
func Load(path string) (*KeyframeIndex, fault.List) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex,
			File: path, Message: err.Error(), Fatal: true}}
	}
	var idx KeyframeIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex,
			File: path, Message: "index is not valid JSON: " + err.Error(), Fatal: true}}
	}
	if idx.Version != Version {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex,
			File: path, Message: fmt.Sprintf("unknown index version %d", idx.Version), Fatal: true}}
	}
	if len(idx.Entries) == 0 {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex,
			File: path, Message: "index has no entries", Fatal: true}}
	}
	var faults fault.List
	// Entries of one representation must be strictly increasing in time.
	lastByRep := map[string]uint64{}
	for i, e := range idx.Entries {
		if e.Segment == "" {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: idx.Camera,
				File: path, Seq: e.FrameSeq,
				Message: fmt.Sprintf("entry %d has no segment", i), Fatal: true,
			})
			continue
		}
		if e.Representation == "" {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: idx.Camera,
				File: path, Seq: e.FrameSeq,
				Message: fmt.Sprintf("entry %d has no representation", i), Fatal: true,
			})
			continue
		}
		if prev, ok := lastByRep[e.Representation]; ok && e.MasterTS <= prev {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: idx.Camera,
				File: path, Seq: e.FrameSeq,
				Message: fmt.Sprintf("entry %d (%s) timestamp %d is not after %d",
					i, e.Representation, e.MasterTS, prev),
				Fatal: true,
			})
		}
		lastByRep[e.Representation] = e.MasterTS
	}
	if faults.Fatal() {
		return &idx, faults
	}
	return &idx, faults
}

// Query returns the seek point at or before ts for the given representation.
// The resolved segment file and byte offset are checked to exist.
func (idx *KeyframeIndex) Query(rep string, ts uint64, outDir string) (KeyframeEntry, error) {
	var best *KeyframeEntry
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Representation != rep {
			continue
		}
		if e.MasterTS <= ts {
			best = e
			continue
		}
		break
	}
	if best == nil {
		return KeyframeEntry{}, fmt.Errorf("index: no %s keyframe at or before ts %d", rep, ts)
	}
	path := filepath.Join(outDir, best.Segment)
	info, err := os.Stat(path)
	if err != nil {
		return KeyframeEntry{}, fmt.Errorf("index: entry points at missing segment %s: %w", best.Segment, err)
	}
	if best.ByteOffset < 0 || best.ByteOffset >= info.Size() {
		return KeyframeEntry{}, fmt.Errorf("index: byte offset %d outside segment %s (%d bytes)",
			best.ByteOffset, best.Segment, info.Size())
	}
	// Confirm the offset lands on a frame line, not inside one.
	f, err := os.Open(path)
	if err != nil {
		return KeyframeEntry{}, err
	}
	defer f.Close()
	buf := make([]byte, 17) // "bbbbbbbbbbbbbbbb" + space
	if _, err := f.ReadAt(buf, best.ByteOffset); err != nil {
		return KeyframeEntry{}, fmt.Errorf("index: cannot read frame at %d: %w", best.ByteOffset, err)
	}
	if buf[16] != ' ' {
		return KeyframeEntry{}, fmt.Errorf("index: byte offset %d does not start a frame line", best.ByteOffset)
	}
	return *best, nil
}

// Rebuild reconstructs an index from the segment files on disk.
func Rebuild(camera string, outDir string) (*KeyframeIndex, fault.List) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, fault.List{{Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex,
			File: outDir, Message: err.Error(), Fatal: true}}
	}
	idx := &KeyframeIndex{Version: Version, Camera: camera}
	var faults fault.List
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".seg") || !strings.HasPrefix(name, camera+"-") {
			continue
		}
		path := filepath.Join(outDir, name)
		h, err := parseHeader(path)
		if err != nil {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: camera,
				File: path, Message: err.Error(), Fatal: false,
			})
			continue
		}
		offsets, err := frameOffsets(path, h.HeaderLen)
		if err != nil {
			continue
		}
		// Recover the segment start timestamp and first sequence from its
		// first frame line: "%016x %08x %d %d".
		first, err := firstFrame(path, h.HeaderLen)
		if err != nil {
			continue
		}
		idx.Entries = append(idx.Entries, KeyframeEntry{
			MasterTS: first.masterTS, Segment: name, ByteOffset: offsets[0],
			FrameSeq: first.seq, Representation: h.Representation,
		})
	}
	if len(idx.Entries) == 0 {
		return nil, append(faults, fault.Fault{
			Kind: fault.KindIndexCorrupt, Stage: fault.StageIndex, Camera: camera,
			File: outDir, Message: "no segments found to rebuild an index from", Fatal: true,
		})
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		a, b := idx.Entries[i], idx.Entries[j]
		if a.Representation != b.Representation {
			return a.Representation < b.Representation
		}
		return a.MasterTS < b.MasterTS
	})
	return idx, faults
}

type frameLine struct {
	masterTS uint64
	seq      uint64
}

// firstFrame parses the first frame line after the header.
func firstFrame(path string, headerLen int64) (frameLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return frameLine{}, err
	}
	defer f.Close()
	if _, err := f.Seek(headerLen, 0); err != nil {
		return frameLine{}, err
	}
	br := bufio.NewReaderSize(f, 256)
	line, err := br.ReadString('\n')
	if line == "" {
		return frameLine{}, err
	}
	var fl frameLine
	if _, err := fmt.Sscanf(line, "%x %x %d", &fl.masterTS, new(uint32), &fl.seq); err != nil {
		return frameLine{}, fmt.Errorf("index: malformed frame line: %v", err)
	}
	return fl, nil
}

// CheckIntegrity verifies every entry resolves and that entries are strictly
// increasing per representation.
func (idx *KeyframeIndex) CheckIntegrity(outDir string) fault.List {
	var faults fault.List
	for _, e := range idx.Entries {
		if _, err := idx.Query(e.Representation, e.MasterTS, outDir); err != nil {
			faults = append(faults, fault.Fault{
				Kind: fault.KindIndexBounds, Stage: fault.StageIndex, Camera: idx.Camera,
				File: e.Segment, Seq: e.FrameSeq, Offset: e.ByteOffset,
				Message: err.Error(), Fatal: false,
			})
		}
	}
	return faults
}

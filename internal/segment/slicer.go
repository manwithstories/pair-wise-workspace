// Package segment plans and writes byte-aligned snapshots of a transport stream.
package segment

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type Boundary struct {
	Offset     int64  `json:"offset"`
	Kind       string `json:"kind"`
	SnapshotID int    `json:"snapshot_id"`
}

type Segment struct {
	Index      int    `json:"index"`
	StartByte  int64  `json:"start_byte"`
	EndByte    int64  `json:"end_byte"`
	Bytes      int64  `json:"bytes"`
	SnapshotID int    `json:"snapshot_id"`
	Boundary   string `json:"boundary,omitempty"`
	File       string `json:"file,omitempty"`
}

type Manifest struct {
	Input    string    `json:"input,omitempty"`
	MinBytes int64     `json:"min_segment_bytes"`
	Segments []Segment `json:"segments"`
	Count    int       `json:"segment_count"`
}

// Plan cuts at version and CC boundaries. Boundaries within MinBytes of the
// start of the current change cluster are coalesced so a burst of PAT/PMT
// updates does not create dozens of tiny files. The final segment may be short.
func Plan(boundaries []Boundary, fileSize, minBytes int64, initialSnapshotID int) []Segment {
	if fileSize <= 0 {
		return nil
	}
	clean := make([]Boundary, 0, len(boundaries))
	for _, b := range boundaries {
		if b.Offset > 0 && b.Offset < fileSize {
			clean = append(clean, b)
		}
	}
	sort.SliceStable(clean, func(i, j int) bool {
		if clean[i].Offset == clean[j].Offset {
			return clean[i].SnapshotID < clean[j].SnapshotID
		}
		return clean[i].Offset < clean[j].Offset
	})
	clean = dedupOffsets(clean)

	representatives := make([]Boundary, 0)
	for i := 0; i < len(clean); {
		start := i
		i++
		for i < len(clean) && clean[i].Offset-clean[start].Offset < minBytes {
			i++
		}
		preferred := clean[start]
		for j := start; j < i; j++ {
			if clean[j].Kind == "version" {
				preferred = clean[j]
				break
			}
		}
		representatives = append(representatives, Boundary{
			Offset: preferred.Offset, Kind: preferred.Kind, SnapshotID: clean[i-1].SnapshotID,
		})
	}

	segments := make([]Segment, 0, len(representatives)+1)
	start := int64(0)
	snapshot := initialSnapshotID
	for _, b := range representatives {
		if b.Offset-start < minBytes {
			continue
		}
		segments = append(segments, Segment{
			Index: len(segments), StartByte: start, EndByte: b.Offset,
			Bytes: b.Offset - start, SnapshotID: snapshot, Boundary: b.Kind,
		})
		start = b.Offset
		snapshot = b.SnapshotID
	}
	segments = append(segments, Segment{
		Index: len(segments), StartByte: start, EndByte: fileSize,
		Bytes: fileSize - start, SnapshotID: snapshot,
	})
	return segments
}

func dedupOffsets(in []Boundary) []Boundary {
	if len(in) == 0 {
		return in
	}
	out := make([]Boundary, 0, len(in))
	for _, b := range in {
		if len(out) > 0 && out[len(out)-1].Offset == b.Offset {
			if b.SnapshotID >= out[len(out)-1].SnapshotID {
				out[len(out)-1] = b
			}
			continue
		}
		out = append(out, b)
	}
	return out
}

// Write copies each planned byte range to a separate .ts file and writes
// manifest.json. It only reads immutable snapshot metadata plus input ranges.
func Write(input, outDir string, manifest Manifest) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()

	for i := range manifest.Segments {
		seg := &manifest.Segments[i]
		seg.File = fmt.Sprintf("segment-%03d.ts", i)
		path := filepath.Join(outDir, seg.File)
		out, err := os.Create(path)
		if err != nil {
			return err
		}
		reader := io.NewSectionReader(in, seg.StartByte, seg.Bytes)
		_, copyErr := io.Copy(out, reader)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	manifest.Count = len(manifest.Segments)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644)
}

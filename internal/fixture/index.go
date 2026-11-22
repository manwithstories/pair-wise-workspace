package fixture

import (
	"encoding/json"
	"fmt"
	"os"
)

// IndexFileName is the shipped keyframe index. The missing_index case omits
// it so the pipeline must rebuild from a segment scan.
const IndexFileName = "keyframes.json"

// IndexManifest is the shipped index shape.
type IndexManifest struct {
	Segments int           `json:"segments"`
	Entries  []IndexRecord `json:"entries"`
}

// IndexRecord is one shipped keyframe position.
type IndexRecord struct {
	URI      string `json:"uri"`
	Start    int64  `json:"start"`
	Keyframe bool   `json:"keyframe"`
}

// indexBytes ships a complete index for every case except missing_index,
// which exercises the rebuild-from-scan path.
func (b *Batch) indexBytes() []byte {
	if b.Case == CaseMissingIdx {
		return nil
	}
	idx := IndexManifest{Segments: len(b.Segments)}
	for i, seg := range b.Segments {
		idx.Entries = append(idx.Entries, IndexRecord{URI: seg.URI, Start: seg.StartTicks, Keyframe: i%3 == 0})
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil
	}
	return data
}

// WriteIndex writes the shipped index, or nothing for missing_index.
func (b *Batch) WriteIndex() error {
	if b.Case == CaseMissingIdx {
		return nil
	}
	return writeSafe(b.Dir, IndexFileName, b.indexBytes())
}

// LoadIndex reads a shipped index, returning nil when it is absent.
func LoadIndex(dir string) (*IndexManifest, error) {
	path, err := SafeJoin(dir, IndexFileName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var idx IndexManifest
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("fixture: invalid keyframe index: %w", err)
	}
	return &idx, nil
}

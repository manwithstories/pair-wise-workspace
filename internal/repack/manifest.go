package repack

import (
	"encoding/json"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// Manifest is the archive index stored as ISO-BMFF (parsed via go-mp4).
type Manifest struct {
	Segments []SegmentInfo `json:"segments"`
}

// WriteManifest writes the segment index as an MP4 file: an ftyp box
// followed by a free box carrying the JSON index. The output structure
// is therefore parseable with a standard ISO-BMFF reader.
func WriteManifest(path string, segs []SegmentInfo) error {
	payload, err := json.Marshal(Manifest{Segments: segs})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "tsbatch-manifest-*.mp4")
	if err != nil {
		return err
	}
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	w := mp4.NewWriter(tmp)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return cleanup(err)
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return cleanup(err)
	}
	if _, err := w.EndBox(); err != nil {
		return cleanup(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFree()}); err != nil {
		return cleanup(err)
	}
	if _, err := mp4.Marshal(w, &mp4.Free{Data: payload}, mp4.Context{}); err != nil {
		return cleanup(err)
	}
	if _, err := w.EndBox(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadManifest parses the MP4 manifest back and returns the per-segment
// hashes, verifying the archive index structure independently.
func ReadManifest(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hashes []string
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type != mp4.BoxTypeFree() {
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		free, ok := box.(*mp4.Free)
		if !ok {
			return nil, nil
		}
		var m Manifest
		if err := json.Unmarshal(free.Data, &m); err != nil {
			return nil, err
		}
		for _, s := range m.Segments {
			hashes = append(hashes, s.SHA256)
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(hashes) == 0 {
		return nil, fmt.Errorf("manifest: no segment index found")
	}
	return hashes, nil
}

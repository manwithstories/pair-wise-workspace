// Package index writes a go-mp4 keyframe index sidecar.
package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"github.com/pairwise/mediapack/internal/cut"
)

// Entry is one keyframe location and its verifiable clip hashes.
type Entry struct {
	ClipID         string       `json:"clip_id"`
	Index          int          `json:"index"`
	Sequence       uint32       `json:"sequence"`
	PTSNanoseconds int64        `json:"pts_ns"`
	PTSTicks       uint64       `json:"pts_ticks"`
	BlobOffset     int          `json:"blob_offset"`
	PSIVersion     int          `json:"psi_version"`
	ClipSHA256     string       `json:"clip_sha256"`
	ContentFP      string       `json:"content_fingerprint"`
	Committable    bool         `json:"committable"`
	Markers        []cut.Marker `json:"markers,omitempty"`
	SilenceStartNS []int64      `json:"silence_start_ns,omitempty"`
	SilenceEndNS   []int64      `json:"silence_end_ns,omitempty"`
}

// Sidecar is the JSON payload carried in an MP4 free box.
type Sidecar struct {
	FormatVersion int     `json:"format_version"`
	Seed          int64   `json:"seed"`
	Entries       []Entry `json:"entries"`
}

// FromSegments builds deterministic index entries.
func FromSegments(seed int64, segments []*cut.Segment) Sidecar {
	sc := Sidecar{FormatVersion: 1, Seed: seed}
	for _, s := range segments {
		e := Entry{
			ClipID: s.ID, Index: s.Index, ClipSHA256: s.ClipSHA256,
			ContentFP: s.ContentFP, Committable: s.Committable, Markers: s.Markers,
		}
		if len(s.Keyframes) > 0 {
			k := s.Keyframes[0]
			e.Sequence = k.Sequence
			e.PTSNanoseconds = int64(k.PTS)
			e.PTSTicks = k.PTSTicks
			e.BlobOffset = k.BlobOffset
			e.PSIVersion = k.PSIVersion
		}
		for _, sil := range s.Silence {
			e.SilenceStartNS = append(e.SilenceStartNS, int64(sil.Start))
			e.SilenceEndNS = append(e.SilenceEndNS, int64(sil.End))
		}
		sc.Entries = append(sc.Entries, e)
	}
	return sc
}

// MarshalSidecar encodes JSON in a valid MP4 free-space box via go-mp4.
func MarshalSidecar(sc Sidecar) ([]byte, error) {
	jsonData, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	n, err := mp4.Marshal(&payload, &mp4.Free{Data: jsonData}, mp4.Context{})
	if err != nil {
		return nil, fmt.Errorf("mp4.Marshal free index: %w", err)
	}
	if n != uint64(payload.Len()) {
		return nil, fmt.Errorf("mp4 wrote %d bytes but buffer has %d", n, payload.Len())
	}
	header := mp4.EncodeBoxInfo(&mp4.BoxInfo{
		Size:       uint64(mp4.SmallHeaderSize) + n,
		HeaderSize: mp4.SmallHeaderSize,
		Type:       mp4.BoxTypeFree(),
	})
	return append(header, payload.Bytes()...), nil
}

// ReadSidecar decodes the first MP4 box when it is a free box containing JSON.
func ReadSidecar(r io.ReadSeeker) (Sidecar, *mp4.BoxInfo, error) {
	bi, err := mp4.ReadBoxInfo(r)
	if err != nil {
		return Sidecar{}, nil, err
	}
	if bi.Type != mp4.BoxTypeFree() {
		return Sidecar{}, bi, fmt.Errorf("index box type %s, want free", bi.Type)
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return Sidecar{}, bi, err
	}
	size := bi.Size - bi.HeaderSize
	if size > 16<<20 {
		return Sidecar{}, bi, fmt.Errorf("index payload too large: %d", size)
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(size)))
	if err != nil {
		return Sidecar{}, bi, err
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return Sidecar{}, bi, fmt.Errorf("decode index JSON: %w", err)
	}
	return sc, bi, nil
}

// ReadSidecarFile reads and decodes a sidecar path.
func ReadSidecarFile(path string) (Sidecar, error) {
	f, err := os.Open(path)
	if err != nil {
		return Sidecar{}, err
	}
	defer f.Close()
	sc, _, err := ReadSidecar(f)
	return sc, err
}

// WriteSidecarFile is primarily a convenience for scripts/tests. Production
// commits should use txn.Manager.Commit with MarshalSidecar for atomicity.
func WriteSidecarFile(path string, sc Sidecar) error {
	b, err := MarshalSidecar(sc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// KeyframeHitRate returns the percentage of entries whose keyframe metadata
// matches the supplied committed hash map.
func KeyframeHitRate(sc Sidecar, committed map[string]string) float64 {
	if len(sc.Entries) == 0 {
		return 0
	}
	hits := 0
	for _, e := range sc.Entries {
		if !e.Committable || e.Sequence == 0 {
			continue
		}
		if got, ok := committed[e.ClipID]; ok && got == e.ClipSHA256 {
			hits++
		}
	}
	return float64(hits) * 100 / float64(len(sc.Entries))
}

// Package index separates container scanning from pipeline execution. An
// index is built once, keyed by the SHA-256 of its input and the index
// schema version; an unchanged input reuses the cached index byte-for-byte.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// IndexVersion is bumped whenever the serialized index schema changes.
const IndexVersion = 3

// MaxInputBytes bounds how much input a single scan will accept.
const MaxInputBytes = 256 << 20 // 256 MiB

// TrackIndex summarizes one scanned track.
type TrackIndex struct {
	ID          uint32 `json:"id"`
	Kind        string `json:"kind"`
	TimebaseNum int64  `json:"timebase_num"`
	TimebaseDen int64  `json:"timebase_den"`
	Packets     int    `json:"packets"`
	Parity      int    `json:"parity"`
	FirstUS     int64  `json:"first_us"`
	LastUS      int64  `json:"last_us"`
	DriftPPM    int    `json:"drift_ppm"`
}

// EventIndex summarizes one timed side-data event.
type EventIndex struct {
	Track     uint32 `json:"track"`
	Kind      string `json:"kind"`
	RawTS     int64  `json:"raw_ts"`
	TrueUS    int64  `json:"true_us"`
	Duration  int64  `json:"duration_us"`
	Truncated bool   `json:"truncated"`
	Offset    int64  `json:"offset"` // first byte offset within the source stream
	Size      int    `json:"size"`
}

// Index is the serializable scan result.
type Index struct {
	Version   int          `json:"version"`
	InputSHA  string       `json:"input_sha256"`
	Seed      int64        `json:"seed"`
	Tracks    []TrackIndex `json:"tracks"`
	Events    []EventIndex `json:"events"`
	Packets   int          `json:"packets"`
	SpanUS    int64        `json:"span_us"`
	BuiltFrom string       `json:"built_from"`
}

// ContentHash returns a stable hash over the index's scan findings, independent
// of cache metadata. Identical inputs must produce identical hashes.
func (idx *Index) ContentHash() string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	// Hash only the deterministic scan payload, not cache bookkeeping.
	payload := struct {
		Version int          `json:"version"`
		Seed    int64        `json:"seed"`
		Tracks  []TrackIndex `json:"tracks"`
		Events  []EventIndex `json:"events"`
		Packets int          `json:"packets"`
		SpanUS  int64        `json:"span_us"`
	}{idx.Version, idx.Seed, idx.Tracks, idx.Events, idx.Packets, idx.SpanUS}
	_ = enc.Encode(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// Scan builds an Index from a generated clip without executing any quality or
// remux work. Event offsets are derived deterministically from the ordered
// packet/event layout rather than real container byte offsets.
func Scan(clip *fixture.Clip, inputSHA string) (*Index, error) {
	if clip == nil {
		return nil, timeline.GlobalError("index.scan", false, fmt.Errorf("nil clip"))
	}
	trackMap := map[uint32]*TrackIndex{}
	for _, tr := range clip.Tracks {
		trackMap[tr.ID] = &TrackIndex{
			ID:          tr.ID,
			Kind:        tr.Kind.String(),
			TimebaseNum: tr.Timebase.Num,
			TimebaseDen: tr.Timebase.Den,
			DriftPPM:    tr.DriftPPM,
			FirstUS:     -1,
		}
	}
	packetCount := 0
	var spanUS int64
	for _, p := range clip.Packets {
		t, ok := trackMap[p.Track]
		if !ok {
			return nil, timeline.TrackError("index.scan", p.Track, p.RawTS,
				timeline.RationalZero, false, fmt.Errorf("unknown track in packet"))
		}
		t.Packets++
		if p.IsParity {
			t.Parity++
		} else {
			packetCount++
		}
		if t.FirstUS < 0 || p.TrueUS < t.FirstUS {
			t.FirstUS = p.TrueUS
		}
		if p.TrueUS > t.LastUS {
			t.LastUS = p.TrueUS
		}
		if p.TrueUS > spanUS {
			spanUS = p.TrueUS
		}
	}
	if len(trackMap) > fixture.MaxTracks {
		return nil, timeline.GlobalError("index.scan", false,
			fmt.Errorf("%w: %d tracks", timeline.ErrLimit, len(trackMap)))
	}

	events := make([]EventIndex, 0, len(clip.Events))
	var offset int64
	for i, ev := range clip.Events {
		events = append(events, EventIndex{
			Track:     ev.Track,
			Kind:      ev.Kind.String(),
			RawTS:     ev.RawTS,
			TrueUS:    ev.TrueUS,
			Duration:  ev.Duration,
			Truncated: ev.Truncated,
			Offset:    offset,
			Size:      len(ev.Payload),
		})
		offset += int64(len(ev.Payload))
		_ = i
	}

	tracks := make([]TrackIndex, 0, len(clip.Tracks))
	for _, tr := range clip.Tracks {
		tracks = append(tracks, *trackMap[tr.ID])
	}
	return &Index{
		Version:   IndexVersion,
		InputSHA:  inputSHA,
		Seed:      clip.Seed,
		Tracks:    tracks,
		Events:    events,
		Packets:   packetCount,
		SpanUS:    spanUS,
		BuiltFrom: "scan",
	}, nil
}

// HashReader streams r through SHA-256 while enforcing the input size limit.
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, MaxInputBytes+1))
	if err != nil {
		return "", 0, timeline.GlobalError("index.hash", false, err)
	}
	if n > MaxInputBytes {
		return "", n, timeline.GlobalError("index.hash", false,
			fmt.Errorf("%w: input larger than %d bytes", timeline.ErrLimit, MaxInputBytes))
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashClip serializes the clip's deterministic packet/event payloads and hashes
// them. It is the in-memory counterpart of hashing a real input file.
func HashClip(clip *fixture.Clip) (string, error) {
	h := sha256.New()
	for _, p := range clip.Packets {
		fmt.Fprintf(h, "%d|%d|%d|%d|%d|", p.Track, p.Seq, p.RawTS, p.TrueUS, p.Duration)
		h.Write(p.Payload)
	}
	for _, ev := range clip.Events {
		fmt.Fprintf(h, "%d|%d|%d|%d|%t|", ev.Track, ev.Kind, ev.RawTS, ev.TrueUS, ev.Truncated)
		h.Write(ev.Payload)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safeJoin ensures candidate stays inside root, rejecting path traversal.
func safeJoin(root, candidate string) (string, error) {
	cleanRoot := filepath.Clean(root)
	cleanCandidate := filepath.Clean(candidate)
	if !filepath.IsAbs(cleanCandidate) {
		cleanCandidate = filepath.Join(cleanRoot, cleanCandidate)
	}
	rel, err := filepath.Rel(cleanRoot, cleanCandidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", timeline.GlobalError("index.path", false,
			fmt.Errorf("%w: %s", timeline.ErrPathEscape, candidate))
	}
	return cleanCandidate, nil
}

// EnsureDir creates root if missing with bounded permissions.
func EnsureDir(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return timeline.GlobalError("index.mkdir", false, err)
	}
	return nil
}

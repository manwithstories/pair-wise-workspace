// Package fixture generates local, deterministic NDJSON timeline material.
// No repository-external media or service is required.
package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/relayteam/relay/internal/timeline"
)

// StreamCount is tracks × bitrate renditions in generated fixtures.
const StreamCount = 6

// TrackNames are the generated source tracks.
var TrackNames = []string{"camera-a", "camera-b"}

// RenditionSpec describes one generated bitrate representation.
type RenditionSpec struct {
	Name       string
	Bandwidth  uint32
	Resolution string
	Codecs     string
}

// Renditions are the shared bitrate layers.
var Renditions = []RenditionSpec{
	{Name: "1080p", Bandwidth: 5_000_000, Resolution: "1920x1080", Codecs: "avc1.640028,mp4a.40.2"},
	{Name: "720p", Bandwidth: 2_500_000, Resolution: "1280x720", Codecs: "avc1.4d401f,mp4a.40.2"},
	{Name: "480p", Bandwidth: 1_000_000, Resolution: "854x480", Codecs: "avc1.42e01e,mp4a.40.2"},
}

// RecordDurationTicks makes 20 records exactly cover one 90 kHz second.
const RecordDurationTicks int64 = 4500

// SegmentTicks is the shared HLS cut duration.
const SegmentTicks int64 = 90_000

// Options controls deterministic fixture generation.
type Options struct {
	Root         string
	Records      int
	PayloadBytes int
}

// Expectation identifies an injected bad record.
type Expectation struct {
	Case       string `json:"case"`
	Path       string `json:"path"`
	RecordNo   int    `json:"first_bad_record"`
	WantCode   string `json:"want_code"`
	WantStages int    `json:"want_stages"`
}

// Set contains generated fixture paths and injected positions.
type Set struct {
	Dir            string        `json:"dir"`
	NormalPath     string        `json:"normal_path"`
	CRCBadPath     string        `json:"crc_bad_path"`
	ReversePath    string        `json:"reverse_path"`
	TruncatedPath  string        `json:"truncated_path"`
	TransitionPath string        `json:"transition_path"`
	BoundaryPath   string        `json:"boundary_path"`
	NormalRecords  int           `json:"normal_records"`
	Expectations   []Expectation `json:"expectations"`
}

// Records builds exactly total deterministic records. total must divide evenly
// across the six track/representation streams so coverage is identical.
func Records(total, payloadBytes int) ([]timeline.Record, error) {
	if total <= 0 {
		return nil, fmt.Errorf("records must be positive")
	}
	if total%StreamCount != 0 {
		return nil, fmt.Errorf("records=%d must be divisible by %d", total, StreamCount)
	}
	if payloadBytes <= 0 {
		payloadBytes = 4096
	}
	perStream := total / StreamCount
	out := make([]timeline.Record, total)
	for i := 0; i < total; i++ {
		stream := i % StreamCount
		track := TrackNames[stream/len(Renditions)]
		rend := Renditions[stream%len(Renditions)]
		seq := i / StreamCount
		clipIndex := seq / 20
		r := timeline.Record{
			Track: track, Rendition: rend.Name, Seq: int64(seq),
			PTS90k: int64(seq) * RecordDurationTicks, Dur90k: RecordDurationTicks,
			Clip:    fmt.Sprintf("clip-%03d", clipIndex),
			Payload: makePayload(i, payloadBytes),
		}
		if seq > 0 && seq%20 == 0 {
			prevClip := fmt.Sprintf("clip-%03d", clipIndex-1)
			r.Transition = &timeline.Transition{
				ID:       fmt.Sprintf("transition-%s-%03d", rend.Name, seq/20),
				Kind:     "fade",
				Endpoint: fmt.Sprintf("edge-node/%s/%s/out", track, rend.Name),
				FromClip: prevClip,
				ToClip:   r.Clip,
			}
		}
		r.ComputeCRC()
		r.RecordNo = i + 1
		r.LineNo = i + 1
		_ = perStream
		out[i] = r
	}
	return out, nil
}

func makePayload(seed, n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	prefix := fmt.Sprintf("relay-%06d-", seed)
	b := make([]byte, n)
	for i := range b {
		if i < len(prefix) {
			b[i] = prefix[i]
		} else {
			b[i] = alphabet[(seed+i)%len(alphabet)]
		}
	}
	return string(b)
}

// WriteNDJSON writes records as one JSON object per line.
func WriteNDJSON(path string, records []timeline.Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for i := range records {
		if err := enc.Encode(&records[i]); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// GenerateAll creates normal, injected-corruption and one-tick boundary files.
func GenerateAll(opts Options) (*Set, error) {
	if opts.Records <= 0 {
		opts.Records = 1200
	}
	records, err := Records(opts.Records, opts.PayloadBytes)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(opts.Root, "fixtures")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	set := &Set{
		Dir: dir, NormalRecords: len(records),
		NormalPath:     filepath.Join(dir, "normal.ndjson"),
		CRCBadPath:     filepath.Join(dir, "crc-bad.ndjson"),
		ReversePath:    filepath.Join(dir, "reverse-time.ndjson"),
		TruncatedPath:  filepath.Join(dir, "truncated-last.ndjson"),
		TransitionPath: filepath.Join(dir, "missing-transition-endpoint.ndjson"),
		BoundaryPath:   filepath.Join(dir, "boundary-one-tick.ndjson"),
	}
	if err := WriteNDJSON(set.NormalPath, records); err != nil {
		return nil, err
	}
	if err := writeCorruptSets(set, records); err != nil {
		return nil, err
	}
	return set, nil
}

// SafeName converts a track/rendition key into a filename component.
func SafeName(key string) string {
	r := strings.NewReplacer("/", "_", " ", "_")
	return r.Replace(key)
}

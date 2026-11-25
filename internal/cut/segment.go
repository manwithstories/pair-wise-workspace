// Package cut builds keyframe-aligned editorial segments.
package cut

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/pairwise/mediapack/internal/demux"
	"github.com/pairwise/mediapack/internal/fingerprint"
	"github.com/pairwise/mediapack/internal/timeline"
)

// KeyframeRef locates a random-access point inside a segment blob.
type KeyframeRef struct {
	Sequence   uint32        `json:"sequence"`
	PTS        time.Duration `json:"pts"`
	PTSTicks   uint64        `json:"pts_ticks"`
	BlobOffset int           `json:"blob_offset"`
	PSIVersion int           `json:"psi_version"`
}

// Marker labels a non-media range such as a CC discontinuity.
type Marker struct {
	Kind   string        `json:"kind"`
	Start  time.Duration `json:"start"`
	End    time.Duration `json:"end"`
	Detail string        `json:"detail,omitempty"`
}

// Segment is one keyframe-aligned clip and its verifiable metadata.
type Segment struct {
	Index        int                 `json:"index"`
	ID           string              `json:"id"`
	Start        time.Duration       `json:"start"`
	End          time.Duration       `json:"end"`
	StartTicks   uint64              `json:"start_ticks"`
	EndTicks     uint64              `json:"end_ticks"`
	KeyFrame     bool                `json:"key_frame"`
	Keyframes    []KeyframeRef       `json:"keyframes"`
	Silence      []timeline.Interval `json:"silence,omitempty"`
	Markers      []Marker            `json:"markers,omitempty"`
	PSIVersions  []int               `json:"psi_versions"`
	VideoFrames  int                 `json:"video_frames"`
	AudioFrames  int                 `json:"audio_frames"`
	AudioPCM     []byte              `json:"-"`
	Video        []demux.Frame       `json:"-"`
	Blob         []byte              `json:"-"`
	ClipSHA256   string              `json:"clip_sha256"`
	ContentFP    string              `json:"content_fingerprint"`
	Committable  bool                `json:"committable"`
	RejectReason string              `json:"reject_reason,omitempty"`
}

// Config controls segment closure.
type Config struct {
	FrameDuration time.Duration
	Source        string
}

// Result contains accepted and rejected segment decisions.
type Result struct {
	Segments []*Segment
	Rejected []*Segment
}

// Build creates keyframe-aligned segments from assembled frames.
func Build(frames []demux.Frame, clock *timeline.Clock, cfg Config) (*Result, error) {
	if cfg.FrameDuration <= 0 {
		cfg.FrameDuration = 40 * time.Millisecond
	}
	ordered := append([]demux.Frame(nil), frames...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].PTSTicks < ordered[j].PTSTicks })

	res := &Result{}
	var cur *Segment
	var blobOffset int
	closeCurrent := func(end time.Duration, endTicks uint64) {
		if cur == nil {
			return
		}
		cur.End, cur.EndTicks = end, endTicks
		cur.Blob = encodeBlob(cur)
		sum := sha256.Sum256(cur.Blob)
		cur.ClipSHA256 = hex.EncodeToString(sum[:])
		chunks := make([]fingerprint.Chunk, 0, len(cur.Video))
		for _, f := range cur.Video {
			chunks = append(chunks, fingerprint.Chunk{PTSTicks: f.PTSTicks, Payload: f.Payload})
		}
		cur.ContentFP = fingerprint.Content(chunks)
		addClockMarkers(cur, clock)
		if err := clock.ValidateSpan(cur.Start, cur.End); err != nil {
			cur.Committable = false
			cur.RejectReason = err.Error()
			res.Rejected = append(res.Rejected, cur)
		} else {
			cur.Committable = true
			res.Segments = append(res.Segments, cur)
		}
		cur = nil
		blobOffset = 0
	}

	for i := range ordered {
		f := ordered[i]
		isVideo := f.Codec == "h264-synthetic"
		if isVideo && f.KeyFrame {
			if cur != nil {
				closeCurrent(f.PTS, f.PTSTicks)
			}
			cur = &Segment{
				Index:       len(res.Segments) + len(res.Rejected),
				ID:          fmt.Sprintf("clip-%04d", len(res.Segments)+len(res.Rejected)+1),
				Start:       f.PTS,
				StartTicks:  f.PTSTicks,
				KeyFrame:    true,
				PSIVersions: []int{f.PSIVersion},
				Video:       []demux.Frame{},
			}
		}
		if cur == nil {
			// Delta/audio before the first keyframe cannot start a reliable clip.
			continue
		}

		if isVideo {
			cur.PSIVersions = addVersion(cur.PSIVersions, f.PSIVersion)
			off := blobOffset
			if f.KeyFrame {
				cur.Keyframes = append(cur.Keyframes, KeyframeRef{
					Sequence: f.Sequence, PTS: f.PTS, PTSTicks: f.PTSTicks,
					BlobOffset: off, PSIVersion: f.PSIVersion,
				})
			}
			cur.VideoFrames++
			cur.Video = append(cur.Video, f)
			blobOffset += 8 + len(f.Payload)
		}
	}

	if cur != nil && len(cur.Video) > 0 {
		last := cur.Video[len(cur.Video)-1]
		endTicks := last.PTSTicks + uint64(cfg.FrameDuration)*timeline.PCRHz/uint64(time.Second)
		closeCurrent(last.PTS+cfg.FrameDuration, endTicks)
	}

	all := append(append([]*Segment(nil), res.Segments...), res.Rejected...)
	for _, f := range ordered {
		if f.Codec != "pcm_s16le" {
			continue
		}
		for _, s := range all {
			if f.PTS >= s.Start && f.PTS < s.End {
				s.AudioFrames++
				s.AudioPCM = append(s.AudioPCM, f.Payload...)
				s.PSIVersions = addVersion(s.PSIVersions, f.PSIVersion)
				break
			}
		}
	}
	return res, nil
}

// encodeBlob deterministically length-frames elementary payloads. Offsets in
// KeyframeRef use the same 8-byte little-endian length prefix framing.
func encodeBlob(s *Segment) []byte {
	var out []byte
	var n [8]byte
	for _, f := range s.Video {
		binary.LittleEndian.PutUint64(n[:], uint64(len(f.Payload)))
		out = append(out, n[:]...)
		out = append(out, f.Payload...)
	}
	return out
}

func addVersion(versions []int, v int) []int {
	if v < 0 {
		return versions
	}
	for _, old := range versions {
		if old == v {
			return versions
		}
	}
	return append(versions, v)
}

func addClockMarkers(s *Segment, clock *timeline.Clock) {
	if clock == nil {
		return
	}
	for _, g := range clock.Gaps {
		if g.Intersects(s.Start, s.End) {
			s.Markers = append(s.Markers, Marker{
				Kind: "cc_gap", Start: g.Start, End: g.End, Detail: g.Reason,
			})
		}
	}
	for _, inv := range clock.Invalid {
		if inv.Intersects(s.Start, s.End) {
			s.Markers = append(s.Markers, Marker{
				Kind: "invalid_clock", Start: inv.Start, End: inv.End, Detail: inv.Reason,
			})
		}
	}
}

// Package fixture generates every input the laboratory needs: DASH and HLS
// manifests, fragmented MP4 segments and deterministic network traces. No
// external files are referenced; all bytes are produced from a seed.
package fixture

import (
	"crypto/sha256"
	"fmt"
	"time"

	"avlab/internal/bmff"
	"avlab/internal/manifest"
	"avlab/internal/media"
	"avlab/internal/transport"
)

// SegmentCount is the canonical number of aligned segments per representation.
const SegmentCount = 8

// Bandwidths are the three DASH/HLS bitrate levels.
var Bandwidths = []struct {
	ID   string
	BW   uint32
	W, H uint32
}{
	{"low", 400_000, 640, 360},
	{"mid", 1_200_000, 1280, 720},
	{"high", 3_000_000, 1920, 1080},
}

// Bundle is the complete generated artifact set.
type Bundle struct {
	Seed       uint64
	DASH       *manifest.Manifest
	HLS        *manifest.Manifest
	Segments   map[string][]byte // key: repID/segmentURL
	SegmentSHA map[string][32]byte
	Trace      *transport.Trace
}

// Generate builds manifests and deterministic fMP4 segments for one seed.
func Generate(seed uint64) (*Bundle, error) {
	segDur := media.FromDuration(2 * time.Second)
	samplesPerSeg := uint32(4)
	sampleDur := uint32(uint64(segDur) / uint64(samplesPerSeg))

	dash := buildManifest(manifest.KindDASH, segDur)
	hls := buildManifest(manifest.KindHLS, segDur)
	applyHLSDiscontinuities(hls)

	b := &Bundle{
		Seed:       seed,
		DASH:       dash,
		HLS:        hls,
		Segments:   map[string][]byte{},
		SegmentSHA: map[string][32]byte{},
		Trace:      transport.NewTrace(seed, 4096),
	}
	for ri, br := range Bandwidths {
		var base media.MediaTime
		for si := 0; si < SegmentCount; si++ {
			seg := dash.Representations[ri].Segments[si]
			spec := bmff.FragmentSpec{
				Sequence: seg.Index,
				TrackID:  1,
				BaseTime: base,
				Samples:  buildSamples(seed, br.ID, seg.Index,
					samplesPerSeg, sampleDur),
			}
			data, err := bmff.BuildFragment(spec)
			if err != nil {
				return nil, err
			}
			if len(data) > media.MaxSegmentBytes {
				return nil, media.NewFault(media.CodeSegmentTooLarge, false,
					"generated segment exceeds 8 MiB")
			}
			key := br.ID + "/" + seg.URL
			b.Segments[key] = data
			b.SegmentSHA[key] = sha256.Sum256(data)
			base += segDur
		}
	}
	return b, nil
}

func buildManifest(kind manifest.Kind, segDur media.MediaTime) *manifest.Manifest {
	m := &manifest.Manifest{
		Kind: kind, TargetTick: segDur,
		MinBufferTick: media.FromDuration(time.Second),
	}
	for _, br := range Bandwidths {
		rep := manifest.Representation{
			ID: br.ID, Bandwidth: br.BW, Width: br.W, Height: br.H,
			MIME: "video/mp4",
		}
		var start media.MediaTime
		for i := uint32(1); i <= SegmentCount; i++ {
			rep.Segments = append(rep.Segments, manifest.Segment{
				Index: i,
				URL:   fmt.Sprintf("%s/seg-%d.m4s", br.ID, i),
				StartTick: start, DurationTick: segDur, Keyframe: true,
			})
			start += segDur
		}
		m.Representations = append(m.Representations, rep)
	}
	return m
}

// applyHLSDiscontinuities opens two new epochs before segments 3 and 6.
func applyHLSDiscontinuities(m *manifest.Manifest) {
	disco := map[uint32]bool{3: true, 6: true}
	for ri := range m.Representations {
		var epoch media.Epoch
		var start media.MediaTime
		for si := range m.Representations[ri].Segments {
			s := &m.Representations[ri].Segments[si]
			if disco[s.Index] {
				epoch++
			}
			s.Epoch = epoch
			s.Discontinuity = disco[s.Index]
			s.StartTick = start
			start += s.DurationTick
		}
	}
}

// buildSamples produces deterministic synthetic sample payloads. The first
// sample of every segment is a sync (keyframe) sample.
func buildSamples(seed uint64, rep string, seg uint32,
	count, dur uint32) []bmff.SampleDesc {
	out := make([]bmff.SampleDesc, count)
	for i := uint32(0); i < count; i++ {
		size := 32 + int(i)*8 + len(rep)*4
		payload := make([]byte, size)
		for k := range payload {
			payload[k] = byte(uint64(seed*31 + uint64(seg)*17 +
				uint64(i)*7 + uint64(k)))
		}
		out[i] = bmff.SampleDesc{
			Size:     uint32(size),
			Duration: dur,
			Keyframe: i == 0,
			Payload:  payload,
		}
	}
	return out
}

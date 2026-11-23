// Package fixture generates deterministic MP4/MOV sample material and
// damaged variants entirely in code — no external files. Used by tests
// and by the demo command.
package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"

	"bcast/demux"
	"bcast/media"
	"bcast/output"

	"github.com/pion/rtp"
)

const (
	VideoTimescale = 90000
	AudioTimescale = 48000
	VideoPerSeg    = 30 // 30 * 3000 = 1s per segment
	AudioPerSeg    = 10 // 10 * 4800 = 1s per segment
	videoDelta     = 3000
	audioDelta     = 4800
	audioBytes     = audioDelta * 2 // int16 PCM
)

// Spec controls program generation.
type Spec struct {
	Seed              int64
	Segments          int
	DupSegments       [2]int // second segment copies the first's content
	SilentSegments    []int  // segments with silent audio
	NoKeyframeSegment int    // segment that starts without an IDR (-1: none)
}

// Assets is one generated delivery batch.
type Assets struct {
	Clean        []byte // intact MP4
	Truncated    []byte // tail cut mid-mdat
	BrokenBox    []byte // trailing box with invalid size
	MissingIndex []byte // stss renamed to free (forces rebuild)
	RTP          []byte // RTP timing sidecar for the clean file
}

// Default returns the demo/acceptance spec.
func Default(seed int64, segments int) Spec {
	return Spec{
		Seed:              seed,
		Segments:          segments,
		DupSegments:       [2]int{3, 7},
		SilentSegments:    []int{2, 9},
		NoKeyframeSegment: 5,
	}
}

// Generate builds the batch deterministically from spec.
func Generate(spec Spec) (*Assets, error) {
	rng := rand.New(rand.NewSource(spec.Seed))
	silent := map[int]bool{}
	for _, s := range spec.SilentSegments {
		silent[s] = true
	}
	// Per-segment content: payload patterns depend on the segment's content
	// key, so duplicated segments produce identical bytes.
	contentSrc := make([]int, spec.Segments)
	for seg := range contentSrc {
		contentSrc[seg] = seg
	}
	if spec.DupSegments[1] >= 0 && spec.DupSegments[1] < spec.Segments {
		contentSrc[spec.DupSegments[1]] = spec.DupSegments[0]
	}
	segContent := make([][]byte, spec.Segments)
	for seg := 0; seg < spec.Segments; seg++ {
		src := contentSrc[seg]
		if src != seg && segContent[src] != nil {
			segContent[seg] = segContent[src]
			continue
		}
		c := make([]byte, 47)
		rng.Read(c)
		segContent[seg] = c
	}
	var vidPayload, audPayload bytes.Buffer
	vid := output.TrackMux{ID: 1, Kind: media.TrackVideo, Timescale: VideoTimescale}
	aud := output.TrackMux{ID: 2, Kind: media.TrackAudio, Timescale: AudioTimescale}
	var rtpPkts []*rtp.Packet
	for seg := 0; seg < spec.Segments; seg++ {
		for i := 0; i < VideoPerSeg; i++ {
			key := i == 0 && seg != spec.NoKeyframeSegment
			nal := byte(0x41)
			if key {
				nal = 0x65
			}
			off := int64(vidPayload.Len())
			var hdr [5]byte
			binary.BigEndian.PutUint32(hdr[:4], 48) // NAL length
			hdr[4] = nal
			vidPayload.Write(hdr[:])
			vidPayload.Write(segContent[seg])
			vid.Samples = append(vid.Samples, output.SampleRef{
				Offset: off, Size: 52, Duration: videoDelta, Keyframe: key,
			})
			rtpPkts = append(rtpPkts, &rtp.Packet{
				Header: rtp.Header{
					Version: 2, SequenceNumber: uint16(len(rtpPkts)),
					Timestamp: uint32((seg*VideoPerSeg + i) * videoDelta),
					SSRC:      VideoTimescale, // clock-rate hint by convention
				},
				Payload: []byte{byte(seg), byte(i)},
			})
		}
		for i := 0; i < AudioPerSeg; i++ {
			off := int64(audPayload.Len())
			buf := make([]byte, audioBytes)
			if !silent[seg] {
				freq := 440.0 + float64(contentSrc[seg])*10
				for j := 0; j < audioDelta; j++ {
					v := int16(8000 * math.Sin(2*math.Pi*freq*float64(j)/AudioTimescale))
					binary.LittleEndian.PutUint16(buf[j*2:], uint16(v))
				}
			}
			audPayload.Write(buf)
			aud.Samples = append(aud.Samples, output.SampleRef{
				Offset: off, Size: audioBytes, Duration: audioDelta,
			})
		}
	}
	// Mux: payloads live in one contiguous buffer per track; concatenate
	// video then audio and shift audio offsets.
	vidEnd := int64(vidPayload.Len())
	for i := range aud.Samples {
		aud.Samples[i].Offset += vidEnd
	}
	src := bytes.NewReader(append(vidPayload.Bytes(), audPayload.Bytes()...))
	clean, err := output.MuxBytes(src, []output.TrackMux{vid, aud})
	if err != nil {
		return nil, fmt.Errorf("fixture mux: %w", err)
	}
	var rtpBuf bytes.Buffer
	if err := demux.WriteRTPSidecar(&rtpBuf, rtpPkts); err != nil {
		return nil, err
	}
	return &Assets{
		Clean:        clean,
		Truncated:    DamageTruncate(clean),
		BrokenBox:    DamageBrokenBox(clean),
		MissingIndex: DamageMissingIndex(clean),
		RTP:          rtpBuf.Bytes(),
	}, nil
}

// DamageTruncate cuts the tail so the last few audio samples are lost.
func DamageTruncate(b []byte) []byte {
	cut := audioBytes*3 + audioBytes/2 // 3 full samples + a partial one
	if cut >= len(b) {
		cut = len(b) / 2
	}
	return b[:len(b)-cut]
}

// DamageBrokenBox appends a box whose declared size is smaller than its
// header — unrecoverable bytes the parser must report, not fabricate over.
func DamageBrokenBox(b []byte) []byte {
	out := append([]byte{}, b...)
	return append(out, 0, 0, 0, 4, 'f', 'r', 'e', 'e')
}

// DamageMissingIndex renames the video stss box to free, dropping the
// declared keyframe index while keeping box sizes consistent.
func DamageMissingIndex(b []byte) []byte {
	out := append([]byte{}, b...)
	off := findBoxType(out, "stss")
	if off < 0 {
		return out
	}
	copy(out[off:off+4], "free")
	return out
}

// findBoxType locates a box's 4-byte type field by walking the box tree.
func findBoxType(b []byte, typ string) int64 {
	var walk func(start, end int64, depth int) int64
	walk = func(start, end int64, depth int) int64 {
		for off := start; off+8 <= end; {
			size := int64(binary.BigEndian.Uint32(b[off:]))
			if size == 1 {
				if off+16 > end {
					return -1
				}
				size = int64(binary.BigEndian.Uint64(b[off+8:]))
			}
			if size == 0 {
				size = end - off
			}
			if size < 8 || off+size > end {
				return -1
			}
			t := string(b[off+4 : off+8])
			if t == typ {
				return off + 4
			}
			hdr := int64(8)
			if size > 8 && binary.BigEndian.Uint32(b[off:]) == 1 {
				hdr = 16
			}
			switch t {
			case "moov", "trak", "mdia", "minf", "stbl":
				if r := walk(off+hdr, off+size, depth+1); r >= 0 {
					return r
				}
			}
			off += size
		}
		return -1
	}
	return walk(0, int64(len(b)), 0)
}

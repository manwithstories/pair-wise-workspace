package main

// Sample generation: deterministic 60s HLS/DASH + RTP fixtures, built
// entirely in-process. Nothing here reads from outside the workdir.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"medrelay/internal/media"
	"medrelay/internal/timeline"
	"medrelay/internal/transport"
)

const (
	segCount        = 10
	segDurMs        = 6000
	audioRate       = 48000
	audioFrameTicks = 960  // 20ms at 48kHz
	videoFrameTicks = 3000 // 30fps at 90kHz
	videoPerSeg     = 180
	audioPerSeg     = 300
	redStride       = 8
	rtpSSRC         = 0x00ABCDEF
)

// xorshift64 deterministic PRNG for sample content.
type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

// genPCM builds one 20ms stereo frame (960 samples x 2ch, interleaved
// little-endian int16). Content depends only on the frame index.
func genPCM(frameIdx int, r *rng) []byte {
	ms := frameIdx * 20
	buf := make([]byte, audioFrameTicks*2*2)
	amp := func(ch int) int16 {
		switch {
		case ms >= 10000 && ms < 12000: // tone, both channels
			return 8000
		case ms >= 20000 && ms < 23000: // full silence, both channels
			return 0
		case ms >= 45000 && ms < 46000: // right loud, left quiet
			if ch == 1 {
				return 6000
			}
			return 20
		default: // background noise below the silence threshold
			return 50
		}
	}
	for i := 0; i < audioFrameTicks; i++ {
		for ch := 0; ch < 2; ch++ {
			a := amp(ch)
			var v int16
			if a > 100 { // square wave
				v = a
				if (i/24)%2 == 0 {
					v = -a
				}
			} else if a > 0 {
				v = int16(r.next() % uint64(2*a+1)) - a
			}
			binary.LittleEndian.PutUint16(buf[(i*2+ch)*2:], uint16(v))
		}
	}
	return buf
}

func videoFrameData(seg, idx int) []byte {
	h := sha256.Sum256([]byte(fmt.Sprintf("v:%d:%d", seg, idx)))
	return h[:16]
}

// videoFrames returns the 180 frames of one segment. Frame 0 of each
// segment is exactly on the segment boundary; later frames carry a small
// deterministic jitter (+-1.5ms) to exercise AV sync measurement.
func videoFrames(seg int) []media.Frame {
	out := make([]media.Frame, 0, videoPerSeg)
	for i := 0; i < videoPerSeg; i++ {
		jitter := int64(0)
		if i > 0 {
			jitter = int64(i%7-3) * 45 // 45 ticks = 0.5ms
		}
		out = append(out, media.Frame{
			Kind: 'v',
			PTS:  int64(seg*videoPerSeg+i)*videoFrameTicks + jitter,
			Dur:  videoFrameTicks,
			TS:   timeline.Video,
			Data: videoFrameData(seg, i),
		})
	}
	return out
}

// rtpStream builds the audio RTP packet stream with redundancy, applies
// the loss burst and a deterministic pairwise reorder, and returns the
// marshaled stream bytes.
func rtpStream(lossBurst int) ([]byte, error) {
	r := &rng{s: 0x5EED1234}
	total := segCount * audioPerSeg
	type pkt struct {
		ts   uint32
		main []byte
	}
	var pkts []pkt
	for i := 0; i < total; i++ {
		pkts = append(pkts, pkt{ts: uint32(i * audioFrameTicks), main: genPCM(i, r)})
	}
	dropped := map[int]bool{}
	for i := 500; i < 500+lossBurst && i < total; i++ {
		dropped[i] = true
	}
	var order []int
	for i := 0; i < total; i++ {
		if !dropped[i] {
			order = append(order, i)
		}
	}
	// Deterministic reorder: swap adjacent pairs at fixed positions.
	for k := 0; k+1 < len(order); k++ {
		if order[k]%101 == 0 && order[k+1] == order[k]+1 {
			order[k], order[k+1] = order[k+1], order[k]
		}
	}
	var out []byte
	for _, i := range order {
		payload := pkts[i].main
		if i >= redStride {
			payload = transport.AppendRedundant(pkts[i].main, pkts[i-redStride].ts, pkts[i-redStride].main)
		}
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: uint16(i),
				Timestamp:      pkts[i].ts,
				SSRC:           rtpSSRC,
				PayloadType:    96,
			},
			Payload: payload,
		}
		b, err := p.Marshal()
		if err != nil {
			return nil, err
		}
		var ln [2]byte
		binary.BigEndian.PutUint16(ln[:], uint16(len(b)))
		out = append(out, ln[:]...)
		out = append(out, b...)
	}
	return out, nil
}

const playlistTmpl = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
`

const mpdTmpl = `<?xml version="1.0"?>
<MPD mediaPresentationDuration="PT60S"><Period><AdaptationSet contentType="video">
<Representation id="v1" bandwidth="800000">
<SegmentTemplate timescale="90000" media="seg-$Number$.m4s" startNumber="0">
<SegmentTimeline>
<S t="0" d="540000" r="9"/>
</SegmentTimeline>
</SegmentTemplate>
</Representation>
</AdaptationSet></Period></MPD>
`

const metaJSON = `{"audio":{"sample_rate":48000,"channels":2,"layout":"stereo"},` +
	`"color":{"primaries":"bt709","transfer":"bt709","matrix":"bt709","full_range":false}}` + "\n"

// generateSample writes the full fixture set into dir/input.
func generateSample(dir string, lossBurst int, corrupt bool) error {
	in := filepath.Join(dir, "input")
	if err := os.MkdirAll(in, 0o755); err != nil {
		return err
	}
	pl := playlistTmpl
	for s := 0; s < segCount; s++ {
		pl += fmt.Sprintf("#EXTINF:6.000,\nseg-%d.m4s\n", s)
	}
	pl += "#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(in, "playlist.m3u8"), []byte(pl), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(in, "manifest.mpd"), []byte(mpdTmpl), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(in, "meta.json"), []byte(metaJSON), 0o644); err != nil {
		return err
	}
	for s := 0; s < segCount; s++ {
		p := filepath.Join(in, fmt.Sprintf("seg-%d.m4s", s))
		if err := writeSegmentFile(p, videoFrames(s)); err != nil {
			return err
		}
		if corrupt && s == 3 {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := f.Write(make([]byte, 100)); err != nil { // truncated garbage
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
	stream, err := rtpStream(lossBurst)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(in, "rtp.stream"), stream, 0o644)
}

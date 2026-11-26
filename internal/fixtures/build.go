// Package fixtures synthesises every sample asset mediad needs. Nothing is
// read from disk beyond the workspace: all MOV/MP4 material, manifests and
// edit lists are generated deterministically from a seed.
package fixtures

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// Case names accepted by the demo command.
const (
	CaseClean     = "clean"
	CaseLateWrap  = "late-wrap"
	CaseTruncated = "truncated"
	CaseInvalidTB = "invalid-base"
)

// Spec describes one generated fixture set.
type Spec struct {
	Root   string
	Seed   int64
	Case   string
	Camera string
}

// Set is the generated fixture inventory.
type Set struct {
	Root        string
	Manifest    string
	Edits       string
	Media       []string
	Case        string
	ExpectError media.ErrorCode
	Notes       []string
}

// Build creates the fixture set on disk and returns its inventory.
func Build(spec Spec) (*Set, error) {
	if spec.Root == "" {
		return nil, media.NewError(media.ErrIO, "fixture root is required")
	}
	if err := os.MkdirAll(spec.Root, 0o755); err != nil {
		return nil, media.NewError(media.ErrIO, "creating fixture root: %v", err)
	}
	set := &Set{Root: spec.Root, Manifest: filepath.Join(spec.Root, "manifest.jsonl"), Edits: filepath.Join(spec.Root, "edits.jsonl"), Case: spec.Case}
	switch spec.Case {
	case CaseClean:
		if err := buildClean(spec, set); err != nil {
			return nil, err
		}
	case CaseLateWrap:
		if err := buildLateWrap(spec, set); err != nil {
			return nil, err
		}
	case CaseTruncated:
		if err := buildTruncated(spec, set); err != nil {
			return nil, err
		}
	case CaseInvalidTB:
		if err := buildInvalidTB(spec, set); err != nil {
			return nil, err
		}
	case CaseMatrix:
		if err := buildMatrix(spec, set); err != nil {
			return nil, err
		}
	case CaseBadLayout:
		if err := buildBadLayout(spec, set); err != nil {
			return nil, err
		}
	default:
		return nil, media.NewError(media.ErrManifest, "unknown fixture case %q", spec.Case)
	}
	return set, nil
}

// writeText writes a text asset atomically inside the fixture root.
func writeText(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.NewError(media.ErrIO, "creating %s: %v", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return media.NewError(media.ErrIO, "writing %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.NewError(media.ErrIO, "publishing %s: %v", path, err)
	}
	return nil
}

// sineAt evaluates a deterministic multi-tone test signal.
func sineAt(seed int64, seconds float64) float64 {
	base := 220 + float64(seed%7)*40
	return 0.28*math.Sin(2*math.Pi*base*seconds) +
		0.12*math.Sin(2*math.Pi*(base*2.7)*seconds) +
		0.05*math.Sin(2*math.Pi*(base*7.3)*seconds+float64(seed%11))
}

// chunkPlan lays out VFR video chunk durations in milliseconds.
func chunkPlan(seed int64, count int) []int64 {
	deltas := []int64{20, 40, 60, 80, 33, 33, 100, 50, 25, 75}
	out := make([]int64, count)
	for i := range out {
		out[i] = deltas[(int(seed)+i)%len(deltas)]
	}
	return out
}

// pcmWrite describes one PCM stream to synthesise.
type pcmWrite struct {
	Rate      int
	Start     int64 // raw ticks in the stream time base
	Samples   int
	Channels  int
	Layout    astiav.ChannelLayout
	Generator func(channel, index int) float64
}

// writeAudioOnlyMOV muxes a single PCM stream into a MOV container. PCM keeps
// the fixture generation dependency-free and lossless, and the pipeline
// exercises its own decode/resample path regardless.
func writeAudioOnlyMOV(path string, p pcmWrite, tb media.Rational) error {
	ofc, err := astiav.AllocOutputFormatContext(nil, "mov", path)
	if err != nil {
		return media.NewError(media.ErrEncode, "allocating fixture context: %v", err)
	}
	defer ofc.Free()
	ofc.SetFlags(ofc.Flags().Add(astiav.FormatContextFlagBitexact))

	codecName := "pcm_s16le"
	codec := astiav.FindEncoderByName(codecName)
	if codec == nil {
		return media.NewError(media.ErrUnsupported, "encoder %s unavailable", codecName)
	}
	cc := astiav.AllocCodecContext(codec)
	defer cc.Free()
	cc.SetSampleRate(p.Rate)
	cc.SetSampleFormat(astiav.SampleFormatS16)
	cc.SetChannelLayout(p.Layout)
	cc.SetTimeBase(astiav.NewRational(int(tb.Num), int(tb.Den)))
	cc.SetThreadCount(1)
	cc.SetFlags(cc.Flags().Add(astiav.CodecContextFlagBitexact))
	if err := cc.Open(codec, nil); err != nil {
		return media.NewError(media.ErrEncode, "opening fixture encoder: %v", err)
	}
	st := ofc.NewStream(nil)
	if st == nil {
		return media.NewError(media.ErrEncode, "cannot create fixture stream")
	}
	if err := st.CodecParameters().FromCodecContext(cc); err != nil {
		return media.NewError(media.ErrEncode, "fixture codec parameters: %v", err)
	}
	st.SetTimeBase(astiav.NewRational(int(tb.Num), int(tb.Den)))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.NewError(media.ErrIO, "creating fixture media dir: %v", err)
	}
	io, err := astiav.OpenIOContext(path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		return media.NewError(media.ErrIO, "opening fixture output: %v", err)
	}
	defer io.Free()
	ofc.SetPb(io)
	if err := ofc.WriteHeader(nil); err != nil {
		return media.NewError(media.ErrEncode, "fixture header: %v", err)
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()
	const block = 1024
	frame.SetSampleRate(p.Rate)
	frame.SetSampleFormat(astiav.SampleFormatS16)
	frame.SetChannelLayout(p.Layout)
	frame.SetNbSamples(block)
	if err := frame.AllocBuffer(1); err != nil {
		return media.NewError(media.ErrEncode, "fixture frame buffer: %v", err)
	}

	emit := func() error {
		for {
			if err := cc.ReceivePacket(pkt); err != nil {
				return nil
			}
			pkt.SetStreamIndex(st.Index())
			pkt.RescaleTs(cc.TimeBase(), st.TimeBase())
			if err := ofc.WriteInterleavedFrame(pkt); err != nil {
				pkt.Unref()
				return media.NewError(media.ErrEncode, "fixture write: %v", err)
			}
			pkt.Unref()
		}
	}

	for pos := 0; pos < p.Samples; pos += block {
		n := block
		if pos+n > p.Samples {
			n = p.Samples - pos
		}
		raw := make([]byte, n*p.Channels*2)
		for i := 0; i < n; i++ {
			for c := 0; c < p.Channels; c++ {
				v := p.Generator(c, pos+i)
				if v > 1 {
					v = 1
				}
				if v < -1 {
					v = -1
				}
				s := int16(math.Round(v * 32767))
				off := (i*p.Channels + c) * 2
				binary.LittleEndian.PutUint16(raw[off:], uint16(s))
			}
		}
		frame.SetNbSamples(n)
		if err := frame.Data().SetBytes(raw, 1); err != nil {
			return media.NewError(media.ErrEncode, "fixture frame fill: %v", err)
		}
		frame.SetPts(p.Start + int64(math.Round(float64(pos)*float64(tb.Num)/float64(tb.Den)*float64(p.Rate))))
		if err := cc.SendFrame(frame); err != nil {
			return media.NewError(media.ErrEncode, "fixture send: %v", err)
		}
		if err := emit(); err != nil {
			return err
		}
	}
	_ = cc.SendFrame(nil)
	if err := emit(); err != nil {
		return err
	}
	if err := ofc.WriteTrailer(); err != nil {
		return media.NewError(media.ErrEncode, "fixture trailer: %v", err)
	}
	return io.Close()
}

// writeAVMOV writes a VFR video track plus a PCM audio track. The video uses
// MPEG-4 Part 2 so the fixture is portable and decodes without external
// codec packs; only the audio track is ever read by the pipeline.
func writeAVMOV(path string, cfg avConfig) error {
	ofc, err := astiav.AllocOutputFormatContext(nil, "mov", path)
	if err != nil {
		return media.NewError(media.ErrEncode, "allocating fixture context: %v", err)
	}
	defer ofc.Free()
	ofc.SetFlags(ofc.Flags().Add(astiav.FormatContextFlagBitexact))

	// Video stream.
	vcodec := astiav.FindEncoderByName("mpeg4")
	if vcodec == nil {
		return media.NewError(media.ErrUnsupported, "mpeg4 encoder unavailable")
	}
	vcc := astiav.AllocCodecContext(vcodec)
	defer vcc.Free()
	vcc.SetWidth(cfg.Width)
	vcc.SetHeight(cfg.Height)
	vcc.SetPixelFormat(astiav.PixelFormatYuv420P)
	vcc.SetTimeBase(astiav.NewRational(1, 1000))
	vcc.SetFramerate(astiav.NewRational(25, 1))
	vcc.SetGopSize(600)
	vcc.SetMaxBFrames(0)
	vcc.SetBitRate(200000)
	vcc.SetThreadCount(1)
	vcc.SetFlags(vcc.Flags().Add(astiav.CodecContextFlagBitexact))
	if err := vcc.Open(vcodec, nil); err != nil {
		return media.NewError(media.ErrEncode, "opening fixture video encoder: %v", err)
	}
	vst := ofc.NewStream(nil)
	if err := vst.CodecParameters().FromCodecContext(vcc); err != nil {
		return media.NewError(media.ErrEncode, "fixture video parameters: %v", err)
	}
	vst.SetTimeBase(astiav.NewRational(1, 1000))

	// Audio stream.
	acodec := astiav.FindEncoderByName("pcm_s16le")
	if acodec == nil {
		return media.NewError(media.ErrUnsupported, "pcm encoder unavailable")
	}
	acc := astiav.AllocCodecContext(acodec)
	defer acc.Free()
	acc.SetSampleRate(cfg.AudioRate)
	acc.SetSampleFormat(astiav.SampleFormatS16)
	acc.SetChannelLayout(cfg.AudioLayout)
	acc.SetTimeBase(astiav.NewRational(1, cfg.AudioRate))
	acc.SetThreadCount(1)
	acc.SetFlags(acc.Flags().Add(astiav.CodecContextFlagBitexact))
	if err := acc.Open(acodec, nil); err != nil {
		return media.NewError(media.ErrEncode, "opening fixture audio encoder: %v", err)
	}
	ast := ofc.NewStream(nil)
	if err := ast.CodecParameters().FromCodecContext(acc); err != nil {
		return media.NewError(media.ErrEncode, "fixture audio parameters: %v", err)
	}
	ast.SetTimeBase(astiav.NewRational(1, cfg.AudioRate))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.NewError(media.ErrIO, "creating fixture media dir: %v", err)
	}
	io, err := astiav.OpenIOContext(path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		return media.NewError(media.ErrIO, "opening fixture output: %v", err)
	}
	defer io.Free()
	ofc.SetPb(io)
	// faststart writes moov before mdat, so a truncated fixture can shrink its
	// mdat payload without invalidating the index that points into it.
	hdr := astiav.NewDictionary()
	defer hdr.Free()
	if err := hdr.Set("movflags", "faststart", astiav.NewDictionaryFlags()); err != nil {
		return media.NewError(media.ErrEncode, "fixture header options: %v", err)
	}
	if err := ofc.WriteHeader(hdr); err != nil {
		return media.NewError(media.ErrEncode, "fixture header: %v", err)
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	emit := func(cc *astiav.CodecContext, st *astiav.Stream) error {
		for {
			if err := cc.ReceivePacket(pkt); err != nil {
				return nil
			}
			pkt.SetStreamIndex(st.Index())
			pkt.RescaleTs(cc.TimeBase(), st.TimeBase())
			if err := ofc.WriteInterleavedFrame(pkt); err != nil {
				pkt.Unref()
				return media.NewError(media.ErrEncode, "fixture write: %v", err)
			}
			pkt.Unref()
		}
	}

	// VFR video: chunk durations in ms, sparse keyframes.
	ptsMS := cfg.VideoStartMS
	channels := cfg.AudioLayout.Channels()
	for i := 0; i < cfg.VideoFrames; i++ {
		f := astiav.AllocFrame()
		f.SetWidth(cfg.Width)
		f.SetHeight(cfg.Height)
		f.SetPixelFormat(astiav.PixelFormatYuv420P)
		f.SetPts(ptsMS)
		if err := f.AllocBuffer(1); err != nil {
			f.Free()
			return media.NewError(media.ErrEncode, "fixture video frame: %v", err)
		}
		raw, err := f.Data().Bytes(1)
		if err != nil {
			f.Free()
			return media.NewError(media.ErrEncode, "fixture video bytes: %v", err)
		}
		for j := range raw {
			raw[j] = byte((j*7 + i*13 + int(cfg.Seed)) % 256)
		}
		if err := f.Data().SetBytes(raw, 1); err != nil {
			f.Free()
			return media.NewError(media.ErrEncode, "fixture video fill: %v", err)
		}
		if err := vcc.SendFrame(f); err != nil {
			f.Free()
			return media.NewError(media.ErrEncode, "fixture video send: %v", err)
		}
		f.Free()
		if err := emit(vcc, vst); err != nil {
			return err
		}
		ptsMS += cfg.VideoDeltasMS[i%len(cfg.VideoDeltasMS)]
	}
	_ = vcc.SendFrame(nil)
	if err := emit(vcc, vst); err != nil {
		return err
	}

	// Audio: start offset then a tone.
	const block = 1024
	frame := astiav.AllocFrame()
	defer frame.Free()
	frame.SetSampleRate(cfg.AudioRate)
	frame.SetSampleFormat(astiav.SampleFormatS16)
	frame.SetChannelLayout(cfg.AudioLayout)
	frame.SetNbSamples(block)
	if err := frame.AllocBuffer(1); err != nil {
		return media.NewError(media.ErrEncode, "fixture audio frame: %v", err)
	}
	for pos := 0; pos < cfg.AudioSamples; pos += block {
		n := block
		if pos+n > cfg.AudioSamples {
			n = cfg.AudioSamples - pos
		}
		raw := make([]byte, n*channels*2)
		for i := 0; i < n; i++ {
			t := float64(pos+i) / float64(cfg.AudioRate)
			for c := 0; c < channels; c++ {
				v := sineAt(cfg.Seed+int64(c), t)
				s := int16(math.Round(v * 32767))
				off := (i*channels + c) * 2
				binary.LittleEndian.PutUint16(raw[off:], uint16(s))
			}
		}
		frame.SetNbSamples(n)
		if err := frame.Data().SetBytes(raw, 1); err != nil {
			return media.NewError(media.ErrEncode, "fixture audio fill: %v", err)
		}
		frame.SetPts(cfg.AudioStart + int64(pos))
		if err := acc.SendFrame(frame); err != nil {
			return media.NewError(media.ErrEncode, "fixture audio send: %v", err)
		}
		if err := emit(acc, ast); err != nil {
			return err
		}
	}
	_ = acc.SendFrame(nil)
	if err := emit(acc, ast); err != nil {
		return err
	}
	if err := ofc.WriteTrailer(); err != nil {
		return media.NewError(media.ErrEncode, "fixture trailer: %v", err)
	}
	return io.Close()
}

// avConfig parameterises the A/V fixture writer.
type avConfig struct {
	Seed          int64
	Width         int
	Height        int
	VideoFrames   int
	VideoStartMS  int64
	VideoDeltasMS []int64
	AudioRate     int
	AudioStart    int64
	AudioSamples  int
	AudioLayout   astiav.ChannelLayout
}

// truncateMP4Sample enlarges the last sample size in the first stsz box,
// simulating a container whose final audio packet is short. The demuxer then
// reports the packet corrupt, which is exactly the recoverable damage the
// pipeline must record and skip.
func truncateMP4Sample(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return media.NewError(media.ErrIO, "reading fixture %s: %v", path, err)
	}
	// Patch the audio track, which is written last, so its final packet
	// declares more bytes than the file actually holds. This is the same
	// signature a recorder leaves behind when it is interrupted mid-write.
	type stszHit struct{ off int }
	var hits []stszHit
	walkBoxes(data, 0, len(data), func(typ string, start, payload, end int) bool {
		if typ != "stsz" || payload+12 > end {
			return true
		}
		sampleSize := binary.BigEndian.Uint32(data[payload+4:])
		count := binary.BigEndian.Uint32(data[payload+8:])
		if sampleSize != 0 || count == 0 {
			return true
		}
		off := payload + 12 + int(count-1)*4
		if off+4 <= end {
			hits = append(hits, stszHit{off: off})
		}
		return true
	})
	patched := false
	var audioOff int
	if len(hits) > 0 {
		off := hits[len(hits)-1].off
		audioOff = off
		old := binary.BigEndian.Uint32(data[off:])
		binary.BigEndian.PutUint32(data[off:], old+(1<<19))
		patched = true
	}
	if patched {
		// Shrink the mdat payload by removing the tail of the last audio
		// packet, which is exactly what an interrupted recorder leaves. The
		// box size is rewritten, so the container stays structurally valid and
		// only the sample data is short.
		if start, size, ok := findBox(data, "mdat"); ok {
			const cut = 1000
			if size-cut > 8 {
				newData := append([]byte(nil), data[:start+size-cut]...)
				newData = append(newData, data[start+size:]...)
				binary.BigEndian.PutUint32(newData[start:], uint32(size-cut))
				_ = audioOff
				data = newData
			}
		}
	}
	if !patched {
		return media.NewError(media.ErrIO, "fixture %s has no patchable stsz", path)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return media.NewError(media.ErrIO, "rewriting fixture %s: %v", path, err)
	}
	return nil
}

// walkBoxes iterates MP4 boxes, descending into containers. Returning false
// stops the walk.
func walkBoxes(b []byte, start, end int, fn func(typ string, start, payload, end int) bool) bool {
	i := start
	for i+8 <= end {
		size := int(binary.BigEndian.Uint32(b[i:]))
		typ := string(b[i+4 : i+8])
		if size == 1 {
			if i+16 > end {
				return true
			}
			size = int(binary.BigEndian.Uint64(b[i+8:]))
		}
		if size < 8 || i+size > end {
			return true
		}
		if !fn(typ, i, i+8, i+size) {
			return false
		}
		switch typ {
		case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "udta":
			if !walkBoxes(b, i+8, i+size, fn) {
				return false
			}
		}
		i += size
	}
	return true
}

var _ = fmt.Sprintf

// findBox returns the payload start and size of the first top-level box with
// the given type.
func findBox(b []byte, want string) (int, int, bool) {
	i := 0
	for i+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[i:]))
		typ := string(b[i+4 : i+8])
		if size == 1 {
			if i+16 > len(b) {
				return 0, 0, false
			}
			size = int(binary.BigEndian.Uint64(b[i+8:]))
		}
		if size < 8 || i+size > len(b) {
			return 0, 0, false
		}
		if typ == want {
			return i, size, true
		}
		i += size
	}
	return 0, 0, false
}

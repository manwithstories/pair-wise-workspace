package fixture

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediarig/internal/audio"
	"mediarig/internal/capture"
	"mediarig/internal/mp4"
	"mediarig/internal/txn"
)

// Options controls deterministic in-repo fixture generation.
type Options struct {
	Root             string
	Rate             int
	Layout           string
	LUFS             float64
	TargetKbps       float64
	Packets          int
	Drop             int
	Reorder          int
	WrapSeq          bool
	WrapTS           bool
	ColorConflict    bool
	TruncateLast     bool
	AudioSSRC        uint32
	VideoSSRC        uint32
	SamplesPerPacket int
}

// Result describes generated logical fixture facts.
type Result struct {
	Paths            *txn.Paths
	AudioSSRC        uint32
	VideoSSRC        uint32
	Packets          int
	SamplesPerPacket int
	VideoFrames      int
	ColorSources     []mp4.ColorMetadata
}

func DefaultOptions(root string) Options {
	return Options{
		Root: root, Rate: 48000, Layout: string(audio.LayoutStereo),
		LUFS: -23, TargetKbps: 1536, Packets: 300,
		AudioSSRC: 0xA1000001, VideoSSRC: 0xB2000002,
		// Zero selects Rate/50 samples per 20 ms packet in Generate.
		SamplesPerPacket: 0,
	}
}

type generatedPacket struct {
	header  rtp.Header
	payload []byte
}

// Generate writes all fixtures with Go-generated L16 and H.264 samples.
func Generate(opts Options) (*Result, error) {
	if opts.Rate == 0 {
		opts.Rate = 48000
	}
	if opts.Layout == "" {
		opts.Layout = string(audio.LayoutStereo)
	}
	layout, err := audio.ParseLayout(opts.Layout)
	if err != nil {
		return nil, err
	}
	if opts.Packets <= 0 {
		opts.Packets = 300
	}
	if opts.SamplesPerPacket <= 0 {
		opts.SamplesPerPacket = opts.Rate / 50 // 20 ms
	}
	if opts.AudioSSRC == 0 {
		opts.AudioSSRC = 0xA1000001
	}
	if opts.VideoSSRC == 0 {
		opts.VideoSSRC = 0xB2000002
	}
	paths, err := txn.Layout(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	if err := txn.RejectSymlinkOutput(paths.InputFile); err != nil {
		return nil, err
	}

	ap := genAudioPackets(opts, layout)
	ap = applyDropAndReorder(ap, opts.Drop, opts.Reorder)
	vp := genVideoPackets(opts)

	f, err := os.Create(paths.InputFile)
	if err != nil {
		return nil, err
	}
	// RTCP first keeps the final physical record an audio RTP packet, which
	// gives truncation reports a stable RTP sequence number.
	if err := writeSR(f, opts.AudioSSRC, ap[0].header.Timestamp, uint32(opts.Packets)); err != nil {
		f.Close()
		return nil, err
	}
	if err := writeSR(f, opts.VideoSSRC, vp[0].header.Timestamp, uint32(len(vp))); err != nil {
		f.Close()
		return nil, err
	}
	for _, p := range vp {
		if err := writeRTPPacket(f, p); err != nil {
			f.Close()
			return nil, err
		}
	}
	for _, p := range ap {
		if err := writeRTPPacket(f, p); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if opts.TruncateLast {
		if err := truncateLastByte(paths.InputFile, 1); err != nil {
			return nil, err
		}
	}

	colors := []mp4.ColorMetadata{{Source: "video-rts", Standard: mp4.ColorBT709, Valid: true}}
	if opts.ColorConflict {
		colors = append(colors, mp4.ColorMetadata{Source: "supply-sdp", Standard: mp4.ColorBT2020, Valid: true})
	}
	info := map[string]any{
		"options":       opts,
		"color_sources": colors,
		"sps":           SPS(),
		"pps":           PPS(),
	}
	if err := txn.WriteJSONAtomic(paths.SourceJSON, info); err != nil {
		return nil, err
	}
	return &Result{
		Paths: paths, AudioSSRC: opts.AudioSSRC, VideoSSRC: opts.VideoSSRC,
		Packets: opts.Packets, SamplesPerPacket: opts.SamplesPerPacket,
		VideoFrames: len(vp), ColorSources: colors,
	}, nil
}

func writeRTPPacket(f *os.File, p generatedPacket) error {
	b, err := (&rtp.Packet{Header: p.header, Payload: p.payload}).Marshal()
	if err != nil {
		return err
	}
	return capture.WriteRecord(f, capture.RecordRTP, b)
}

func genAudioPackets(opts Options, layout audio.Layout) []generatedPacket {
	ch := layout.Channels()
	startSeq := uint16(100)
	startTS := uint32(1000)
	if opts.WrapSeq {
		startSeq = 65520
	}
	if opts.WrapTS {
		startTS = math.MaxUint32 - uint32(opts.SamplesPerPacket*2)
	}
	out := make([]generatedPacket, opts.Packets)
	for i := 0; i < opts.Packets; i++ {
		payload := make([]byte, opts.SamplesPerPacket*ch*2)
		for n := 0; n < opts.SamplesPerPacket; n++ {
			frame := i*opts.SamplesPerPacket + n
			for c := 0; c < ch; c++ {
				// Deterministic channel-specific harmonics stay below full
				// scale and make downmix/layout tests meaningful.
				freq := 330 + float64(c)*55
				amp := 0.16 + 0.02*float64(c%3)
				v := amp * math.Sin(2*math.Pi*freq*float64(frame)/float64(opts.Rate))
				s := int16(math.Round(v * 32767))
				o := (n*ch + c) * 2
				binary.BigEndian.PutUint16(payload[o:], uint16(s))
			}
		}
		out[i] = generatedPacket{
			header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: startSeq + uint16(i),
				Timestamp: startTS + uint32(i*opts.SamplesPerPacket), SSRC: opts.AudioSSRC,
			},
			payload: payload,
		}
	}
	return out
}

func applyDropAndReorder(in []generatedPacket, drop, reorder int) []generatedPacket {
	n := len(in)
	dropSet := map[int]bool{}
	for d := 0; d < drop && d < n-4; d++ {
		idx := (d + 1) * n / (drop + 1)
		// Keep drops isolated and away from adjacent swap positions.
		for dropSet[idx] || (idx > 0 && dropSet[idx-1]) || (idx+1 < n && dropSet[idx+1]) {
			idx++
			if idx >= n-1 {
				idx = 1
			}
		}
		dropSet[idx] = true
	}
	out := make([]generatedPacket, 0, n-drop)
	for i, p := range in {
		if !dropSet[i] {
			out = append(out, p)
		}
	}
	// Reorder in the filtered slice at spaced, non-overlapping adjacent pairs.
	m := len(out)
	for r := 0; r < reorder && r*4+2 < m; r++ {
		i := 2 + r*(m/(reorder+1))
		if i+1 < m {
			out[i], out[i+1] = out[i+1], out[i]
		}
	}
	return out
}

func genVideoPackets(opts Options) []generatedPacket {
	const frames = 180
	startSeq := uint16(400)
	startTS := uint32(70000)
	if opts.WrapSeq {
		startSeq = 65530
	}
	if opts.WrapTS {
		startTS = math.MaxUint32 - 6000
	}
	out := make([]generatedPacket, frames)
	for i := 0; i < frames; i++ {
		nal := []byte{0x41, 0x9a, byte(i), 0x10, 0x20}
		marker := false
		if i == 0 {
			nal = IDR()
			marker = true
		}
		out[i] = generatedPacket{
			header: rtp.Header{
				Version: 2, PayloadType: 97, SequenceNumber: startSeq + uint16(i),
				Timestamp: startTS + uint32(i*3000), SSRC: opts.VideoSSRC, Marker: marker,
			},
			payload: nal,
		}
	}
	return out
}

func writeSR(f *os.File, ssrc, rtpTS uint32, packetCount uint32) error {
	b := make([]byte, 28)
	b[0] = 0x80
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6) // 7 32-bit words total
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint32(b[8:12], 0xe645a000) // fixed NTP MSW
	binary.BigEndian.PutUint32(b[12:16], 0)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	binary.BigEndian.PutUint32(b[20:24], packetCount)
	binary.BigEndian.PutUint32(b[24:28], packetCount*100)
	return capture.WriteRecord(f, capture.RecordRTCP, b)
}

func truncateLastByte(path string, n int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.Truncate(path, st.Size()-n)
}

// SPS returns a small fixed constrained-baseline-style configuration.
func SPS() []byte {
	return []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
}

// PPS returns a small fixed picture parameter set.
func PPS() []byte { return []byte{0x68, 0xce, 0x38, 0x80} }

// IDR returns a deterministic synthetic IDR NAL unit.
func IDR() []byte {
	return []byte{0x65, 0x88, 0x80, 0x40, 0xde, 0xad, 0xbe, 0xef}
}

var _ = filepath.Join

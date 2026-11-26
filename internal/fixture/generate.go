// Package fixture generates the deterministic, fully local demo asset:
// moving-shape I420 video frames, 48 kHz tone audio, XOR parity packets and
// a reproducible mix of checksum corruption, burst loss, reordering and
// duplicates. Nothing is downloaded or read from outside the output dir.
package fixture

import (
	"crypto/sha256"
	"math"
	"math/rand"

	"avpipe/internal/media"
	"avpipe/internal/transport"
)

// Source geometry of the generated master.
const (
	Width  = 640
	Height = 360
)

// FrameSet is one generated video frame with its raw I420 planes.
type FrameSet struct {
	Frame media.Frame
}

// AudioSet is one 1024-sample audio packet's PCM payload.
type AudioSet struct {
	Seq uint32
	PTS uint64
	PCM []byte
}

// GenerateFrames builds frames deterministic moving graphics: a drifting
// gradient background plus a moving bright block, so compression has real
// motion to encode and PSNR is measurable rather than infinite.
func GenerateFrames(count uint32, seed int64) []FrameSet {
	rng := rand.New(rand.NewSource(seed))
	out := make([]FrameSet, count)
	phase := rng.Float64() * math.Pi * 2
	for f := uint32(0); f < count; f++ {
		y := make([]byte, Width*Height)
		cb := make([]byte, (Width/2)*(Height/2))
		cr := make([]byte, (Width/2)*(Height/2))
		t := float64(f) / float64(media.VideoFPS)
		boxX := float64(Width)*0.5 + math.Sin(t*2.0+phase)*float64(Width)*0.25
		boxY := float64(Height)*0.5 + math.Cos(t*1.7+phase)*float64(Height)*0.2
		// Smooth, low-frequency content: a slow diagonal luma gradient plus
		// one soft-edged moving block. It stays compressible at 180 kbps
		// while retaining real frame-to-frame motion for PSNR measurement.
		for row := 0; row < Height; row++ {
			for col := 0; col < Width; col++ {
				base := 50.0 + 70.0*float64(col)/float64(Width) +
					40.0*float64(row)/float64(Height)
				dx := float64(col) - boxX
				dy := float64(row) - boxY
				dist := math.Sqrt(dx*dx + dy*dy)
				soft := 90.0 / (1.0 + dist*dist/900.0)
				v := base + soft
				if v < 0 {
					v = 0
				} else if v > 235 {
					v = 235
				}
				y[row*Width+col] = byte(v + 0.5)
			}
		}
		for row := 0; row < Height/2; row++ {
			for col := 0; col < Width/2; col++ {
				cb[row*(Width/2)+col] = byte(118 + 8*math.Sin(t+float64(col)/32.0))
				cr[row*(Width/2)+col] = byte(122 + 8*math.Cos(t+float64(row)/24.0))
			}
		}
		pts := media.VideoPTS(f)
		packed := append(append(append([]byte(nil), y...), cb...), cr...)
		sum := sha256.Sum256(packed)
		out[f] = FrameSet{Frame: media.Frame{
			Seq: f, PTS: pts, Width: Width, Height: Height,
			Y: y, Cb: cb, Cr: cr, SHA256: sum[:],
		}}
	}
	return out
}

// GenerateAudio builds AudioPacketsPerSecond packets per second, each with
// AudioSamplesPerPacket 48 kHz mono samples, so eight packets cover exactly
// one second and form one FEC block.
func GenerateAudio(packets uint32, seed int64) []AudioSet {
	rng := rand.New(rand.NewSource(seed + 7))
	out := make([]AudioSet, packets)
	for p := uint32(0); p < packets; p++ {
		firstSample := int(p) * media.AudioSamplesPerPacket
		pcm := make([]byte, media.AudioSamplesPerPacket*2)
		freq := 220.0 + rng.Float64()*120.0
		for i := 0; i < media.AudioSamplesPerPacket; i++ {
			sample := int(0.28 * 32767 * math.Sin(2*math.Pi*freq*float64(firstSample+i)/float64(media.AudioRate)))
			pcm[2*i] = byte(int16(sample))
			pcm[2*i+1] = byte(int16(sample) >> 8)
		}
		sum := sha256.Sum256(pcm)
		out[p] = AudioSet{Seq: p, PTS: media.AudioPTS(firstSample), PCM: pcm}
		_ = sum
	}
	return out
}

// FrameContent packs an I420 frame the same way the checksum was computed.
func FrameContent(f *media.Frame) []byte {
	return append(append(append([]byte(nil), f.Y...), f.Cb...), f.Cr...)
}

// WrapVideo wraps a frame as a transport media packet.
func WrapVideo(f *media.Frame) media.Packet {
	content := FrameContent(f)
	seq := f.Seq
	block := seq / transport.BlockSize
	wrapped := transport.WrapPayload(media.KindVideo, seq, f.PTS, content)
	sum := sha256.Sum256(wrapped)
	return media.Packet{Seq: seq, BlockID: block, Kind: media.KindVideo,
		PTS: f.PTS, Payload: wrapped, SHA256: sum[:]}
}

// WrapAudio wraps an audio packet as a transport media packet.
func WrapAudio(a AudioSet) media.Packet {
	seq := a.Seq
	block := seq / transport.BlockSize
	wrapped := transport.WrapPayload(media.KindAudio, seq, a.PTS, a.PCM)
	sum := sha256.Sum256(wrapped)
	return media.Packet{Seq: seq, BlockID: block, Kind: media.KindAudio,
		PTS: a.PTS, Payload: wrapped, SHA256: sum[:]}
}

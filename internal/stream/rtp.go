package stream

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"os"

	pionrtp "github.com/pion/rtp"

	"mediaroute/internal/audio"
	"mediaroute/internal/clock"
	"mediaroute/internal/index"
	"mediaroute/internal/rtp"
)

// chunkData is the result of reading one bounded batch of samples.
type chunkData struct {
	// payloads maps the local sample index (within the batch) to its bytes.
	payloads map[int][]byte
	// readErr records that a sample declared readable could not be read at
	// runtime, so the caller can isolate it instead of silently skipping it.
	readErr map[int]error
	// budgetStop is set when the byte budget ended the batch early; the caller
	// must reschedule the remaining samples rather than advance past them.
	budgetStop bool
	// total and peak describe allocation volume for the memory metric.
	total int
	peak  int
}

// readChunk reads a bounded batch of samples. Unreadable samples are skipped
// (they carry no payload), read failures are reported per sample, and the
// batch stops cleanly when the next sample would exceed MaxChunkBytes.
func readChunk(ctx context.Context, f *os.File, samples []index.Sample) (*chunkData, error) {
	out := &chunkData{payloads: map[int][]byte{}, readErr: map[int]error{}}
	for i, s := range samples {
		if err := ctx.Err(); err != nil {
			return out, canceled(err)
		}
		if !s.Readable || s.Size == 0 {
			continue
		}
		if int(s.Size) > MaxChunkBytes-out.total {
			out.budgetStop = true
			break
		}
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, s.Offset); err != nil {
			out.readErr[i] = err
			continue
		}
		out.payloads[i] = buf
		out.total += len(buf)
		if len(buf) > out.peak {
			out.peak = len(buf)
		}
	}
	return out, nil
}

// pcmFromBytes interprets a demo PCM payload as little-endian int16 mono and
// scales it into float32. Real MP4 payloads would be decoded by a codec; the
// fixtures carry raw PCM so the pipeline is fully self-contained.
func pcmFromBytes(b []byte) []float32 {
	n := len(b) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(b[i*2 : i*2+2]))
		out[i] = float32(v) / 32768
	}
	return out
}

// packetize cuts a stereo buffer into fixed-size RTP packets.
func packetize(samples []float32, framesPerPacket int, seq *uint16) []*pionrtp.Packet {
	perPacket := framesPerPacket * audio.TargetChannels
	n := (len(samples) + perPacket - 1) / perPacket
	packets := make([]*pionrtp.Packet, 0, n)
	for i := 0; i < n; i++ {
		start := i * perPacket
		end := start + perPacket
		if end > len(samples) {
			end = len(samples)
		}
		payload := make([]byte, (end-start)*2)
		for j := start; j < end; j++ {
			v := int16(math.Round(float64(samples[j]) * 32767))
			binary.LittleEndian.PutUint16(payload[(j-start)*2:], uint16(v))
		}
		packets = append(packets, &pionrtp.Packet{
			Header:  pionrtp.Header{SequenceNumber: *seq, Timestamp: uint32(i * framesPerPacket)},
			Payload: payload,
		})
		*seq++
	}
	return packets
}

// simulateNetwork feeds packets to the recoverer in a deterministically
// shuffled order that mimics a lossy link: local reordering is injected so the
// recovery module has something to recover.
func simulateNetwork(ctx context.Context, packets []*pionrtp.Packet, rec **rtp.Recoverer, res *Result, sink Sink, at clock.Rational, lossBurst int, burstDropped *bool) error {
	if len(packets) == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(int64(len(packets)) * 31))
	// Swap each packet with one of the next few to create bounded reordering.
	shuffled := append([]*pionrtp.Packet(nil), packets...)
	for i := 0; i+3 < len(shuffled); i += 4 {
		j := i + 1 + rng.Intn(2)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	burstStart := -1
	if lossBurst > 0 && !*burstDropped && len(shuffled) > lossBurst {
		burstStart = len(shuffled)/2 - lossBurst/2
		*burstDropped = true
	}
	for i, p := range shuffled {
		if err := ctx.Err(); err != nil {
			return canceled(err)
		}
		if burstStart >= 0 && i >= burstStart && i < burstStart+lossBurst {
			continue
		}
		out, err := (*rec).Push(p)
		if err != nil {
			return err
		}
		for _, q := range out {
			sink(Event{
				Kind: EventRTP, TrackID: 0, Source: at,
				Output:  clock.Rational{Num: int64(q.Timestamp), Den: audio.TargetRate},
				Bytes:   len(q.Payload),
				Latency: 0,
			})
		}
	}
	return nil
}

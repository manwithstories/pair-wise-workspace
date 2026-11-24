package stream

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"time"

	pionrtp "github.com/pion/rtp"

	"mediaroute/internal/audio"
	"mediaroute/internal/clock"
	"mediaroute/internal/index"
	"mediaroute/internal/rtp"
)

// readChunk reads a bounded batch of samples, returning per-sample payloads
// and the peak allocation size. Unreadable samples yield nil payloads; the
// reader never errors for a bad range, so isolation is handled by the caller.
func readChunk(ctx context.Context, f *os.File, samples []index.Sample) (map[int][]byte, int, int, error) {
	out := make(map[int][]byte, len(samples))
	peak := 0
	total := 0
	for i, s := range samples {
		if err := ctx.Err(); err != nil {
			return nil, 0, peak, canceled(err)
		}
		if !s.Readable || s.Size == 0 {
			continue
		}
		if int(s.Size) > MaxChunkBytes-total {
			// Would exceed the chunk budget; leave it for the next batch by
			// returning what we have. The caller re-reads from the sample list.
			break
		}
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, s.Offset); err != nil {
			// Treat as unreadable rather than a fatal error.
			continue
		}
		out[i] = buf
		total += len(buf)
		if len(buf) > peak {
			peak = len(buf)
		}
	}
	return out, total, peak, nil
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
func simulateNetwork(ctx context.Context, packets []*pionrtp.Packet, rec **rtp.Recoverer, res *Result, sink Sink, at clock.Rational) error {
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
	for _, p := range shuffled {
		if err := ctx.Err(); err != nil {
			return canceled(err)
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

var _ = time.Now

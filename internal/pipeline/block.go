package pipeline

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/archive/rtprestore/internal/audio"
	"github.com/archive/rtprestore/internal/checkpoint"
	"github.com/archive/rtprestore/internal/clock"
	"github.com/archive/rtprestore/internal/index"
	"github.com/archive/rtprestore/internal/rtp"
	"github.com/archive/rtprestore/internal/video"
)

// block is the bounded in-memory work unit. It holds packets, completed
// frames and queued audio samples, and serializes into one deterministic
// shard. Memory scales with BlockSize, never with total capture length.
type block struct {
	packets      []rtp.OrderedPacket
	packetTimes  []clock.StreamTime
	frames       []*video.Frame
	audioSamples []int16
	silence      int
	clipped      int
	preserved    int
}

func newBlock() *block { return &block{} }

func (b *block) len() int { return len(b.packets) }

func (b *block) addPacket(op rtp.OrderedPacket, st clock.StreamTime) {
	b.packets = append(b.packets, op)
	b.packetTimes = append(b.packetTimes, st)
}

func (b *block) addFrame(f *video.Frame) { b.frames = append(b.frames, f) }

// queueAudio extracts 16-bit samples from the synthetic audio payload. The
// synthetic mono samples occupy payload bytes after the 7-byte header, two
// bytes per sample.
func (b *block) queueAudio(op rtp.OrderedPacket) {
	p := op.Packet.Payload
	if len(p) <= synthHeaderLen {
		return
	}
	for i := synthHeaderLen; i+1 < len(p); i += 2 {
		v := int16(binary.LittleEndian.Uint16(p[i : i+2]))
		b.audioSamples = append(b.audioSamples, v)
	}
}

const synthHeaderLen = 7

// flushAudio converts and normalizes the block's audio once, recording the
// archive-required silence/clipping/preservation statistics.
func flushAudio(b *block) {
	if len(b.audioSamples) == 0 {
		return
	}
	in := audio.Frame{SampleRate: synthAudioRate, Channels: 1, Samples: b.audioSamples}
	conv, br, err := audio.Convert(in)
	if err != nil {
		return // malformed fragment: counted nowhere, source stays intact
	}
	if br.Preserved {
		b.preserved += len(b.audioSamples)
		return
	}
	out, st := audio.Normalize(conv)
	b.silence += st.SilenceFrames
	b.clipped += st.ClipFrames
	b.audioSamples = out.Samples
}

const synthAudioRate = 48000

func (b *block) audioCounters() (silence, clipped, preserved int) {
	return b.silence, b.clipped, b.preserved
}

// serialize produces a deterministic byte representation. Every multibyte
// field is fixed-endian and map iteration is avoided, so identical blocks
// hash identically across runs.
func (b *block) serialize() []byte {
	var buf bytes.Buffer
	writeU32 := func(v uint32) {
		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], v)
		buf.Write(tmp[:])
	}
	writeU64 := func(v uint64) {
		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], v)
		buf.Write(tmp[:])
	}
	writeU32(uint32(len(b.packets)))
	for _, op := range b.packets {
		writeU32(op.SSRC)
		writeU32(uint32(op.Seq))
		writeU32(op.Packet.Timestamp)
		writeU32(uint32(len(op.Packet.Payload)))
		buf.Write(op.Packet.Payload)
	}
	writeU32(uint32(len(b.frames)))
	for _, f := range b.frames {
		writeU32(f.ID)
		writeU64(uint64(f.PTSUs))
		var flag uint32
		if f.Keyframe {
			flag |= 1
		}
		if f.Bad {
			flag |= 2
		}
		writeU32(flag)
	}
	writeU32(uint32(len(b.audioSamples)))
	var tmp [2]byte
	for _, s := range b.audioSamples {
		binary.LittleEndian.PutUint16(tmp[:], uint16(s))
		buf.Write(tmp[:])
	}
	return buf.Bytes()
}

// finalizeBlock wraps the serialized block in an mp4ff ftyp container and
// writes one shard, then checkpoints so the cursor only advances on durable
// success.
func finalizeBlock(store *checkpoint.Store, b *block, ki *index.KeyframeIndexer,
	tl *clock.Timeline, cfg Config, cursor int64) error {
	body := b.serialize()
	var container bytes.Buffer
	ftyp := mp4.CreateFtyp()
	if err := ftyp.Encode(&container); err != nil {
		return fmt.Errorf("pipeline: ftyp encode: %w", err)
	}
	if _, err := container.Write(body); err != nil {
		return err
	}
	idx := len(store.State().Shards)
	if _, err := store.WriteShard(idx, &container); err != nil {
		return err
	}
	return store.Advance(cursor, tl.Version(), b.len())
}

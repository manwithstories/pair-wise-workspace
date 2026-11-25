package synth

import (
	"bytes"

	"archivectl/internal/container"
	"archivectl/internal/transport"
)

const chunkBodySize = 256

type packetizedSegment struct {
	ID       uint32
	Frames   [][]byte
	StartSeq uint16
}

func packetizeSegment(id uint32, data []byte, startSeq uint16, timestamp uint32) (packetizedSegment, uint16, error) {
	seg := packetizedSegment{ID: id, StartSeq: startSeq}
	seq := startSeq
	if len(data) == 0 {
		h := transport.ChunkHeader{Flags: transport.ChunkFlagStart | transport.ChunkFlagEnd, Index: 0, SegmentID: id}.Marshal()
		frame, err := transport.BuildRTPPacket(seq, timestamp, 41, true, h)
		if err != nil {
			return seg, seq, err
		}
		seg.Frames = append(seg.Frames, frame)
		return seg, seq + 1, nil
	}
	for off, index := 0, uint16(0); off < len(data); off += chunkBodySize {
		end := off + chunkBodySize
		if end > len(data) {
			end = len(data)
		}
		flags := byte(0)
		if off == 0 {
			flags |= transport.ChunkFlagStart
		}
		if end == len(data) {
			flags |= transport.ChunkFlagEnd
		}
		h := transport.ChunkHeader{Flags: flags, Index: index, SegmentID: id}.Marshal()
		payload := append(append([]byte{}, h...), data[off:end]...)
		frame, err := transport.BuildRTPPacket(seq, timestamp, 41, end == len(data), payload)
		if err != nil {
			return seg, seq, err
		}
		seg.Frames = append(seg.Frames, frame)
		seq++
		index++
	}
	return seg, seq, nil
}

func fixtureSegments(seed int64) ([][]byte, []packetizedSegment, error) {
	init := container.BuildInitSegment([]container.Track{
		{ID: 1, Kind: container.KindVideo, Codec: "avc1", Timescale: videoTimescale},
		{ID: 2, Kind: container.KindSubtitle, Codec: "tx3g", Timescale: 1000},
	})
	blobs := [][]byte{init}
	metas := make([]packetizedSegment, 0, 5)
	seq := uint16(0)
	// Callers override starting sequence by rebuilding; keep this helper for
	// blob construction only.
	_ = seq
	for i := 0; i < 4; i++ {
		frag, err := container.BuildFragment(uint32(i+1), fragmentTrack(seed, i))
		if err != nil {
			return nil, nil, err
		}
		blobs = append(blobs, frag)
	}
	return blobs, metas, nil
}

func packetizeAll(seed int64, startSeq uint16) ([][]byte, []packetizedSegment, error) {
	blobs, _, err := fixtureSegments(seed)
	if err != nil {
		return nil, nil, err
	}
	seq := startSeq
	metas := make([]packetizedSegment, 0, len(blobs))
	frames := make([][]byte, 0)
	for i, blob := range blobs {
		ts := uint32(0)
		if i > 0 {
			ts = uint32((i - 1) * 6000)
		}
		meta, next, err := packetizeSegment(uint32(i+1), blob, seq, ts)
		if err != nil {
			return nil, nil, err
		}
		metas = append(metas, meta)
		frames = append(frames, meta.Frames...)
		seq = next
	}
	return frames, metas, nil
}

// WrapCapture creates arrival-order packets with adjacent reordering,
// duplicates and a 16-bit sequence wrap near 65535.
func WrapCapture(seed int64) ([][]byte, error) {
	frames, _, err := packetizeAll(seed, 65530)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(frames)+2)
	for i := 0; i < len(frames); i++ {
		// Swap pairs at two bounded locations.
		if i == 3 || i == 9 {
			out = append(out, frames[i+1], frames[i])
			i++
			continue
		}
		out = append(out, frames[i])
		// Duplicate two packets shortly after their originals.
		if i == 1 || i == 7 {
			out = append(out, frames[i])
		}
	}
	return out, nil
}

// BurstCapture drops a consecutive three-packet burst from the third media
// fragment. Sequence headers retain their original numbers, giving an exact
// gap ledger; no replacement media is generated.
func BurstCapture(seed int64) ([][]byte, []int64, error) {
	frames, metas, err := packetizeAll(seed, 100)
	if err != nil {
		return nil, nil, err
	}
	// metas indexes: 0 init, 1 frag1, 2 frag2, 3 frag3 (damaged), 4 frag4.
	damaged := metas[3]
	dropSeqs := map[uint16]bool{
		damaged.StartSeq + 1: true,
		damaged.StartSeq + 2: true,
		damaged.StartSeq + 3: true,
	}
	dropped := make([]int64, 0, 3)
	out := make([][]byte, 0, len(frames)-3)
	for i, f := range frames {
		// Parse the low two bytes by locating RTP sequence through pion helper
		// indirectly: packetize order means frame index corresponds to seq.
		seq := uint16(100 + i)
		if dropSeqs[seq] {
			dropped = append(dropped, int64(seq))
			continue
		}
		out = append(out, f)
	}
	return out, dropped, nil
}

// WriteRTPCapture is a small wrapper used by generation and tests.
func WriteRTPCapture(path string, frames [][]byte) error {
	var buf bytes.Buffer
	if err := transport.WriteCapture(&buf, frames); err != nil {
		return err
	}
	return writeFile(path, buf.Bytes())
}

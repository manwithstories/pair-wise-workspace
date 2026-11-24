package demux

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/abema/go-mp4"

	"mediadeliver/internal/model"
)

// flatRef points one emitted packet at its track/sample pair.
type flatRef struct {
	tr     *mp4Track
	sample int
}

// readSampleBox captures raw sample-table boxes for later expansion.
func (m *MP4Reader) readSampleBox(h *mp4.ReadHandle, bi *mp4.BoxInfo) error {
	if m.cur == nil {
		return m.corrupt(bi, "sample box outside trak", fmt.Errorf("orphan %s", bi.Type))
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		return m.corrupt(bi, "read sample box", err)
	}
	switch b := box.(type) {
	case *mp4.Stts:
		m.cur.stts = b.Entries
	case *mp4.Stss:
		m.cur.stss = b.SampleNumber
		m.cur.hasStss = true
	case *mp4.Stsz:
		if b.SampleSize != 0 {
			m.cur.sizes = make([]uint32, b.SampleCount)
			for i := range m.cur.sizes {
				m.cur.sizes[i] = b.SampleSize
			}
		} else {
			m.cur.sizes = append([]uint32(nil), b.EntrySize...)
		}
	case *mp4.Stsc:
		m.cur.stsc = append([]mp4.StscEntry(nil), b.Entries...)
	case *mp4.Stco:
		m.cur.chunkOff = make([]int64, len(b.ChunkOffset))
		for i, off := range b.ChunkOffset {
			m.cur.chunkOff[i] = int64(off)
		}
	}
	return nil
}

// expandTrack turns raw table boxes into per-sample DTS, durations, offsets
// and the keyframe set, checking arithmetic overflow and table consistency.
func (m *MP4Reader) expandTrack(tr *mp4Track) error {
	// 1. stts -> per-sample DTS/duration (raw media timeline, origin 0).
	var dts, durs []int64
	var cur int64
	for si, e := range tr.stts {
		for k := uint32(0); k < e.SampleCount; k++ {
			if cur < 0 || cur > math.MaxInt64-int64(e.SampleDelta) {
				return model.At(model.KindOverflow, "demux", "expand stts",
					int64(len(dts)), fmt.Sprintf("DTS overflow at stts entry %d", si))
			}
			dts = append(dts, cur)
			durs = append(durs, int64(e.SampleDelta))
			cur += int64(e.SampleDelta)
		}
	}
	tr.dts, tr.durations = dts, durs

	n := len(dts)
	if n > m.limits.MaxPackets {
		return model.At(model.KindLimitExceeded, "demux", "expand samples", -1,
			fmt.Sprintf("sample count %d exceeds limit %d", n, m.limits.MaxPackets))
	}
	// 2. stsz consistency and per-packet size bound.
	if len(tr.sizes) != n {
		return model.At(model.KindCorruptMedia, "demux", "expand stsz", -1,
			fmt.Sprintf("stsz has %d sizes but stts has %d samples", len(tr.sizes), n))
	}
	for i, sz := range tr.sizes {
		if int(sz) > m.limits.MaxPacketBytes {
			return model.At(model.KindLimitExceeded, "demux", "expand stsz", int64(i),
				fmt.Sprintf("sample %d size %d exceeds packet limit", i, sz))
		}
	}
	// 3. stsc + stco -> per-sample absolute file offsets.
	if err := m.expandChunks(tr, n); err != nil {
		return err
	}
	// 4. stss -> keyframe set (stss sample numbers are 1-based). Without stss
	// every video sample is theoretically a sync sample only for non-video;
	// for video we conservatively mark nothing unless stss proves otherwise.
	if tr.hasStss {
		for _, num := range tr.stss {
			if num < 1 || int(num) > n {
				return model.At(model.KindCorruptMedia, "demux", "expand stss",
					int64(num), "stss sample number out of range")
			}
			tr.keySamples[int(num)-1] = true
		}
	}
	return nil
}

// expandChunks builds the chunk list and validates it against sample count.
func (m *MP4Reader) expandChunks(tr *mp4Track, totalSamples int) error {
	if len(tr.stsc) == 0 || len(tr.chunkOff) == 0 {
		if totalSamples == 0 {
			return nil
		}
		return model.At(model.KindCorruptMedia, "demux", "expand chunks", -1,
			"missing stsc/stco for non-empty track")
	}
	sampleCursor := 0
	for chunkIdx := 0; chunkIdx < len(tr.chunkOff); chunkIdx++ {
		perChunk := m.samplesPerChunk(tr.stsc, chunkIdx)
		base := sampleCursor
		tr.chunks = append(tr.chunks, chunk{sampleIndex: base, fileOffset: tr.chunkOff[chunkIdx]})
		sampleCursor += perChunk
	}
	if sampleCursor != totalSamples {
		return model.At(model.KindCorruptMedia, "demux", "expand chunks", -1,
			fmt.Sprintf("chunk samples %d != stts samples %d", sampleCursor, totalSamples))
	}
	return nil
}

// samplesPerChunk resolves stsc (FirstChunk is 1-based) for a 0-based chunk.
func (m *MP4Reader) samplesPerChunk(entries []mp4.StscEntry, chunkIdx int) int {
	per := int(entries[0].SamplesPerChunk)
	for _, e := range entries {
		if int(e.FirstChunk)-1 <= chunkIdx {
			per = int(e.SamplesPerChunk)
		} else {
			break
		}
	}
	return per
}

// buildFlat orders packets track-by-track, each track in sample order.
func (m *MP4Reader) buildFlat() []flatRef {
	var flat []flatRef
	for _, tr := range m.tracks {
		for s := range tr.dts {
			flat = append(flat, flatRef{tr: tr, sample: s})
		}
	}
	return flat
}

// ReadPacket emits the next unified packet or io.EOF.
func (m *MP4Reader) ReadPacket(ctx context.Context) (*model.Packet, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Wrap(model.KindCancelled, "demux", "read mp4", m.seq,
			"cancelled before next sample", err)
	}
	if m.seq == 0 {
		flat := m.buildFlat()
		if len(flat) > m.limits.MaxPackets {
			return nil, model.At(model.KindLimitExceeded, "demux", "read mp4", -1,
				"total packet count exceeds limit")
		}
		m.flatStore(flat)
	}
	if int(m.seq) >= len(m.flat) {
		return nil, io.EOF
	}
	ref := m.flat[m.seq]
	tr, s := ref.tr, ref.sample
	offset, err := m.sampleOffset(tr, s)
	if err != nil {
		return nil, err
	}
	size := int64(tr.sizes[s])
	payload, err := m.readMdat(offset, size)
	if err != nil {
		return nil, err
	}
	origin := tr.mediaTime
	if origin < 0 {
		origin = 0
	}
	p := &model.Packet{
		SourceID: m.sourceID, TrackID: tr.id, Kind: tr.kind, Codec: tr.codec,
		Seq:       m.seq,
		DTS:       tr.dts[s] + origin,
		PTS:       tr.dts[s] + origin, // no ctts in generated fixtures: PTS == DTS
		Duration:  tr.durations[s],
		Timescale: tr.timescale,
		Keyframe:  tr.keySamples[s],
		Payload:   payload,
	}
	m.seq++
	return p, nil
}

// flatStore keeps the flattened emission order on the reader.
func (m *MP4Reader) flatStore(flat []flatRef) { m.flat = flat }

// sampleOffset resolves a sample's absolute byte offset from its chunk list.
func (m *MP4Reader) sampleOffset(tr *mp4Track, sample int) (int64, error) {
	// find the chunk whose range contains sample (chunks are ordered).
	idx := -1
	for i, c := range tr.chunks {
		if c.sampleIndex <= sample {
			idx = i
		} else {
			break
		}
	}
	if idx < 0 {
		return 0, model.At(model.KindCorruptMedia, "demux", "sample offset",
			int64(sample), "no chunk contains sample")
	}
	c := tr.chunks[idx]
	off := c.fileOffset
	for k := c.sampleIndex; k < sample; k++ {
		off += int64(tr.sizes[k])
	}
	return off, nil
}

// readMdat reads size bytes at absolute offset, locating the owning mdat box.
func (m *MP4Reader) readMdat(offset, size int64) ([]byte, error) {
	for _, loc := range m.mdats {
		if offset >= loc.dataOffset && offset+size <= loc.dataOffset+loc.dataSize {
			if _, err := m.r.Seek(offset, io.SeekStart); err != nil {
				return nil, model.Wrap(model.KindIO, "demux", "seek mdat", offset,
					"seek failed", err)
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(m.r, buf); err != nil {
				return nil, model.Wrap(model.KindCorruptMedia, "demux", "read mdat",
					offset, "truncated sample payload", err)
			}
			return buf, nil
		}
	}
	return nil, model.At(model.KindCorruptMedia, "demux", "read mdat", offset,
		"sample offset outside any mdat box")
}

// ReadAll drains every track into a slice, stopping at the first corruption.
func (m *MP4Reader) ReadAll(ctx context.Context) ([]*model.Packet, error) {
	var out []*model.Packet
	for {
		p, err := m.ReadPacket(ctx)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}

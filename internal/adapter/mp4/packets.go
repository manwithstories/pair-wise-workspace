package mp4

import (
	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// buildPackets turns the expanded sample tables into kernel packets. Encoded
// bytes are only materialised when opts.KeepPayload is set; otherwise packets
// carry offsets and sizes so the kernel can stream them on demand. Either way
// the hash is computed from the encoded bytes, never from a re-encode.
func (p *parser) buildPackets() error {
	asset := p.asset
	for _, entry := range p.tables {
		trk := entry.track
		tbl := entry.table
		var prevDTS int64
		havePrev := false
		for i := int64(0); i < tbl.count; i++ {
			// Decode time must be strictly non-decreasing within a track. A
			// backwards jump means the sample table's timing wrapped or is
			// corrupt; surface it with the offending sample's byte offset.
			if havePrev && tbl.dts[i] < prevDTS {
				return p.errTrack(fault.CodeTimestampWrap, tbl.offsets[i], trk.ID,
					"track %d sample %d DTS %dns precedes previous %dns",
					trk.ID, i, tbl.dts[i], prevDTS)
			}
			prevDTS, havePrev = tbl.dts[i], true
			pkt := media.Packet{
				TrackIndex:    trk.Index,
				SourceTrackID: trk.ID,
				PacketIndex:   i,
				SourceOffset:  tbl.offsets[i],
				SourceSize:    int64(tbl.sizes[i]),
				DTSNS:         tbl.dts[i],
				PTSNS:         tbl.pts[i],
				DurationNS:    tbl.dur[i],
				Sync:          tbl.sync[i],
				Sequence:      uint32(i),
			}
			if p.opts.KeepPayload {
				buf, err := p.sr.read(tbl.offsets[i], int64(tbl.sizes[i]))
				if err != nil {
					return p.errTrack(fault.CodeCorruptContainer, tbl.offsets[i], trk.ID,
						"read sample %d failed: %v", i, err)
				}
				pkt.Payload = buf
			}
			asset.Packets = append(asset.Packets, pkt)
		}
	}
	return nil
}

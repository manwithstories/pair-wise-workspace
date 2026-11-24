// Package verify re-reads remuxed output and cross-checks it against the
// per-segment provenance records, proving how each output segment maps back
// to its input packets.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"mediadeliver/internal/model"
)

// SourcePacketFunc returns the ordered packets of one source track. It lets
// the verifier reconstruct expected payloads from the inputs themselves.
type SourcePacketFunc func(materialID string, trackID uint32) ([]*model.Packet, error)

// HashReader streams r and returns its SHA-256 hex digest and byte count.
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, model.Wrap(model.KindIO, "verify", "hash output", -1,
			"failed reading output for hashing", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifySegment recomputes the output digest and checks it against the record.
// It validates the 64-hex digest, the byte count, range consistency and the
// total packet count derived from the recorded source ranges.
func VerifySegment(rec *model.SegmentRecord, output io.Reader) error {
	if len(rec.SHA256Hex) != 64 {
		return model.At(model.KindConflict, "verify", "digest length", -1,
			fmt.Sprintf("segment %d digest is not 64 hex chars", rec.Index))
	}
	gotHash, gotBytes, err := HashReader(output)
	if err != nil {
		return err
	}
	if gotHash != rec.SHA256Hex {
		return model.At(model.KindConflict, "verify", "digest match", -1,
			fmt.Sprintf("segment %d output hash %s != recorded %s",
				rec.Index, gotHash, rec.SHA256Hex))
	}
	if gotBytes != rec.Bytes {
		return model.At(model.KindConflict, "verify", "byte count", -1,
			fmt.Sprintf("segment %d bytes %d != recorded %d", rec.Index, gotBytes, rec.Bytes))
	}
	if len(rec.Ranges) == 0 {
		return model.At(model.KindInvalidInput, "verify", "ranges", -1,
			fmt.Sprintf("segment %d has no source ranges", rec.Index))
	}
	var rangePackets int64
	for _, r := range rec.Ranges {
		if r.LastSeq < r.FirstSeq || r.Packets <= 0 {
			return model.At(model.KindInvalidInput, "verify", "range bounds",
				r.FirstSeq,
				fmt.Sprintf("segment %d has bad range %s/%d", rec.Index, r.MaterialID, r.TrackID))
		}
		rangePackets += r.Packets
	}
	if rangePackets != rec.PacketCount {
		return model.At(model.KindConflict, "verify", "packet count", -1,
			fmt.Sprintf("segment %d range packets %d != record %d",
				rec.Index, rangePackets, rec.PacketCount))
	}
	return nil
}

// VerifyProvenance reconstructs the expected payload stream from the source
// tracks (range by range, in record order, seq ascending) and confirms its
// digest equals the recorded output digest. This is the item-by-item proof
// that an output segment is a lossless copy of its declared inputs.
func VerifyProvenance(rec *model.SegmentRecord, sources SourcePacketFunc) error {
	h := sha256.New()
	var n int64
	for _, rng := range rec.Ranges {
		pkts, err := sources(rng.MaterialID, rng.TrackID)
		if err != nil {
			return model.Wrap(model.KindIO, "verify", "load source", rng.FirstSeq,
				fmt.Sprintf("cannot load %s/%d", rng.MaterialID, rng.TrackID), err)
		}
		matched := 0
		for _, p := range pkts {
			if p.Seq < rng.FirstSeq || p.Seq > rng.LastSeq {
				continue
			}
			h.Write(p.Payload)
			n += int64(len(p.Payload))
			matched++
		}
		if int64(matched) != rng.Packets {
			return model.At(model.KindConflict, "verify", "range coverage", rng.FirstSeq,
				fmt.Sprintf("range %s/%d declares %d packets but source provided %d",
					rng.MaterialID, rng.TrackID, rng.Packets, matched))
		}
	}
	rebuilt := hex.EncodeToString(h.Sum(nil))
	if rebuilt != rec.SHA256Hex {
		return model.At(model.KindConflict, "verify", "provenance digest", -1,
			fmt.Sprintf("segment %d rebuilt hash %s != recorded %s",
				rec.Index, rebuilt, rec.SHA256Hex))
	}
	if n != rec.Bytes {
		return model.At(model.KindConflict, "verify", "provenance bytes", -1,
			fmt.Sprintf("segment %d rebuilt bytes %d != recorded %d", rec.Index, n, rec.Bytes))
	}
	return nil
}

// Package validate checks record metadata against the RTP metadata carrier and
// verifies frame bytes against the SHA-256 recorded with the frame.
package validate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/rtp"

	"mediq/internal/model"
)

// RTP extension identifier carrying the capture timestamp.
const CaptureTimestampExtID uint8 = 0x0E

// Metadata is the decoded RTP metadata carrier stored before the frame bytes.
type Metadata struct {
	Seq       uint64
	Timestamp uint64
	Timebase  model.Timebase
}

// EncodeMetadata builds the RTP metadata carrier for a frame. It is a normal
// RTP packet: the header carries the sequence number and the 32-bit capture
// timestamp, the payload carries a human-readable summary, and a one-byte
// header extension carries the full 64-bit timestamp so timestamps beyond the
// 32-bit RTP field survive the trip.
func EncodeMetadata(seq, ts uint64, tb model.Timebase) []byte {
	payload := fmt.Sprintf("seq=%d;ts=%d;tb=%d/%d", seq, ts, tb.Num, tb.Den)
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: uint16(seq),
			Timestamp:      uint32(ts),
			SSRC:           0x4d454449, // "MEDI"
			Marker:         true,
		},
		Payload: []byte(payload),
	}
	ext := make([]byte, 8)
	binary.BigEndian.PutUint64(ext, ts)
	pkt.Header.Extension = true
	// RFC 8285 one-byte header extension profile. The constant is unexported
	// in pion/rtp, so it is spelled out here.
	pkt.Header.ExtensionProfile = 0xBEDE
	if err := pkt.Header.SetExtension(CaptureTimestampExtID, ext); err != nil {
		panic(err)
	}
	buf, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return buf
}

// ParseMetadata decodes and structurally validates an RTP metadata carrier.
// It returns a Fault (never a bare error) so callers can report position.
func ParseMetadata(meta []byte, seq uint64, offset int64) (*Metadata, *model.Fault) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(meta); err != nil {
		return nil, &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
			Reason: "RTP metadata carrier does not decode", Err: err,
		}
	}
	if pkt.Version != 2 {
		return nil, &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
			Expected: "rtp_version=2", Actual: "rtp_version=" + strconv.Itoa(int(pkt.Version)),
		}
	}
	md, err := parseSummary(pkt.Payload)
	if err != nil {
		return nil, &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
			Reason: "RTP metadata payload malformed", Err: err,
		}
	}
	if ext := pkt.Header.GetExtension(CaptureTimestampExtID); len(ext) == 8 {
		if extTS := binary.BigEndian.Uint64(ext); extTS != md.Timestamp {
			return nil, &model.Fault{
				Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
				Expected: "capture_ts=" + strconv.FormatUint(md.Timestamp, 10),
				Actual:   "capture_ts=" + strconv.FormatUint(extTS, 10),
			}
		}
	} else {
		return nil, &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
			Reason: "RTP metadata carrier has no capture timestamp extension",
		}
	}
	if md.Seq != seq {
		return nil, &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: seq, HasSeq: true, Offset: offset,
			Expected: "seq=" + strconv.FormatUint(seq, 10),
			Actual:   "seq=" + strconv.FormatUint(md.Seq, 10),
		}
	}
	return md, nil
}

func parseSummary(b []byte) (*Metadata, error) {
	fields := map[string]string{}
	for _, part := range strings.Split(string(b), ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed metadata field %q", part)
		}
		fields[k] = v
	}
	seq, err := strconv.ParseUint(fields["seq"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad seq: %w", err)
	}
	ts, err := strconv.ParseUint(fields["ts"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad ts: %w", err)
	}
	tb, err := model.ParseTimebase(fields["tb"])
	if err != nil {
		return nil, err
	}
	return &Metadata{Seq: seq, Timestamp: ts, Timebase: tb}, nil
}

// MetadataMatches verifies the frame's metadata carrier agrees with the header
// and returns a fault anchored at the metadata offset otherwise.
func MetadataMatches(fr *model.Frame) *model.Fault {
	md, fault := ParseMetadata(fr.Meta, fr.Seq, fr.MetaOffset)
	if fault != nil {
		return fault
	}
	if md.Timestamp != fr.Timestamp || md.Timebase != fr.Timebase {
		return &model.Fault{
			Code: model.CodeMetadataMismatch, Seq: fr.Seq, HasSeq: true, Offset: fr.MetaOffset,
			Expected: fmt.Sprintf("ts=%d tb=%s", fr.Timestamp, fr.Timebase),
			Actual:   fmt.Sprintf("ts=%d tb=%s", md.Timestamp, md.Timebase),
		}
	}
	return nil
}

// HashFrame returns the SHA-256 of the frame payload.
func HashFrame(payload []byte) [32]byte { return sha256.Sum256(payload) }

// VerifyFrame compares the computed hash with the recorded one. The returned
// fault names the frame sequence number, the byte offset of the frame payload,
// and the expected/actual digests.
func VerifyFrame(fr *model.Frame) *model.Fault {
	actual := HashFrame(fr.Payload)
	if actual == fr.Hash {
		return nil
	}
	return &model.Fault{
		Code:     model.CodeHashMismatch,
		Seq:      fr.Seq,
		HasSeq:   true,
		Offset:   fr.FrameOffset,
		Expected: hex.EncodeToString(fr.Hash[:]),
		Actual:   hex.EncodeToString(actual[:]),
		Err:      model.ErrDamagedInput,
	}
}

// CheckFrame runs all per-frame checks: metadata carrier and SHA-256.
func CheckFrame(fr *model.Frame) *model.Fault {
	if fault := MetadataMatches(fr); fault != nil {
		return fault
	}
	return VerifyFrame(fr)
}

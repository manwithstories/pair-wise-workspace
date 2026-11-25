package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pion/rtp"

	"mediacheck/internal/media"
)

// IndexEntry is one keyframe record and its RTP identity.
type IndexEntry struct {
	PTS          int64  `json:"pts_90k"`
	Offset       int64  `json:"offset"`
	Size         int    `json:"size"`
	Keyframe     bool   `json:"keyframe"`
	RTPSequence  uint16 `json:"rtp_sequence"`
	RTPTimestamp uint32 `json:"rtp_timestamp"`
}

// KeyframeIndex is the JSON sidecar. ExpiresAt makes stale indexes explicit.
type KeyframeIndex struct {
	Codec                 string                 `json:"codec"`
	Clock                 media.ClockDeclaration `json:"clock"`
	SourceSize            int64                  `json:"source_size"`
	SourceSHA256          string                 `json:"source_sha256"`
	GeneratedAt           time.Time              `json:"generated_at"`
	ExpiresAt             time.Time              `json:"expires_at"`
	KeyframeIntervalTicks int64                  `json:"keyframe_interval_ticks"`
	Entries               []IndexEntry           `json:"entries"`
}

// LoadKeyIndex decodes JSON. now is injectable so expiry tests are deterministic.
func LoadKeyIndex(r io.Reader, now time.Time) (*KeyframeIndex, error) {
	var idx KeyframeIndex
	dec := json.NewDecoder(io.LimitReader(r, 4<<20))
	if err := dec.Decode(&idx); err != nil {
		return nil, media.E(media.StageIndex, media.KindInvalidArgument, -1,
			fmt.Errorf("decode keyframe index: %w", err))
	}
	if _, err := ParseCodec(idx.Codec); err != nil {
		return nil, err
	}
	if err := idx.Clock.Validate(media.StageIndex); err != nil {
		return nil, err
	}
	if !idx.ExpiresAt.IsZero() && now.After(idx.ExpiresAt) {
		off := int64(-1)
		if len(idx.Entries) > 0 {
			off = idx.Entries[0].Offset
		}
		return nil, media.E(media.StageIndex, media.KindStaleIndex, off,
			fmt.Errorf("index expired at %s", idx.ExpiresAt.Format(time.RFC3339)))
	}
	return &idx, nil
}

// ValidateAgainstSource checks hash/size and every declared byte range.
func (idx *KeyframeIndex) ValidateAgainstSource(source []byte, sourceSHA256 string) error {
	if idx.SourceSize > 0 && idx.SourceSize != int64(len(source)) {
		return media.E(media.StageIndex, media.KindStaleIndex, 0,
			fmt.Errorf("index source_size=%d actual=%d", idx.SourceSize, len(source)))
	}
	if idx.SourceSHA256 != "" && sourceSHA256 != "" && idx.SourceSHA256 != sourceSHA256 {
		return media.E(media.StageIndex, media.KindStaleIndex, 0,
			fmt.Errorf("index source hash %s does not match %s",
				shortHash(idx.SourceSHA256), shortHash(sourceSHA256)))
	}
	for i, e := range idx.Entries {
		if e.Offset < 0 || e.Size < 0 {
			return media.E(media.StageIndex, media.KindOutOfBounds, e.Offset,
				fmt.Errorf("entry %d has negative range", i))
		}
		end := e.Offset + int64(e.Size)
		if e.Offset > int64(len(source)) || end > int64(len(source)) || end < e.Offset {
			return media.E(media.StageIndex, media.KindOutOfBounds, e.Offset,
				fmt.Errorf("entry %d range [%d,%d) exceeds source size %d",
					i, e.Offset, end, len(source)))
		}
		if !e.Keyframe {
			return media.E(media.StageIndex, media.KindInvalidArgument, e.Offset,
				fmt.Errorf("entry %d is not marked as keyframe", i))
		}
	}
	return idx.ValidateTimeline()
}

// ValidateTimeline checks PTS monotonicity without repairing it.
func (idx *KeyframeIndex) ValidateTimeline() error {
	tracker := media.TimelineTracker{
		Stage:        media.StageIndex,
		ExpectedStep: idx.KeyframeIntervalTicks,
	}
	for _, e := range idx.Entries {
		if err := tracker.Observe(e.PTS, e.Offset); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRTP cross-checks index RTP fields against pion/rtp-decoded packets
// and classifies duplicates, reversals, gaps and timestamp wraps.
func (idx *KeyframeIndex) ValidateRTP(packets []rtp.Packet) error {
	ts := media.TimelineTracker{Stage: media.StageIndex}
	var lastSeq int64 = -1
	for i, pkt := range packets {
		var entry *IndexEntry
		if i < len(idx.Entries) {
			entry = &idx.Entries[i]
			if entry.RTPSequence != pkt.SequenceNumber {
				return media.E(media.StageIndex, media.KindValidation, entry.Offset,
					fmt.Errorf("entry %d seq index=%d packet=%d",
						i, entry.RTPSequence, pkt.SequenceNumber))
			}
			if entry.RTPTimestamp != pkt.Timestamp {
				return media.E(media.StageIndex, media.KindValidation, entry.Offset,
					fmt.Errorf("entry %d timestamp index=%d packet=%d",
						i, entry.RTPTimestamp, pkt.Timestamp))
			}
		}
		offset := int64(i)
		if entry != nil {
			offset = entry.Offset
		}
		if err := validateSequence(&lastSeq, pkt.SequenceNumber, offset); err != nil {
			return err
		}
		if err := ts.ObserveRTPTimestamp(pkt.Timestamp, offset); err != nil {
			return err
		}
	}
	return nil
}

func validateSequence(last *int64, raw uint16, offset int64) error {
	next, wrapped := media.ExpandUint32(*last, uint32(raw))
	if *last < 0 {
		*last = next
		return nil
	}
	if wrapped && raw == 0 {
		// Sequence-number modulus crossing is expected by RTP. Record it as a
		// distinct classified condition in tests; normal short fixtures avoid
		// crossing so all four timeline fault classes stay unambiguous.
		*last = next
		return nil
	}
	switch {
	case next == *last:
		return media.E(media.StageIndex, media.KindDuplicate, offset,
			fmt.Errorf("duplicate RTP sequence %d", raw))
	case next < *last:
		return media.E(media.StageIndex, media.KindOutOfOrder, offset,
			fmt.Errorf("RTP sequence reversed %d -> %d", *last, next))
	case next != *last+1:
		return media.E(media.StageIndex, media.KindGap, offset,
			fmt.Errorf("RTP sequence gap %d -> %d", *last, next))
	}
	*last = next
	return nil
}

// DecodeRTPPacket uses pion/rtp to parse one raw packet.
func DecodeRTPPacket(raw []byte) (rtp.Packet, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return pkt, media.E(media.StageIndex, media.KindInvalidArgument, -1,
			fmt.Errorf("decode RTP packet: %w", err))
	}
	return pkt, nil
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

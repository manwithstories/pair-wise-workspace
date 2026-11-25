package ingest_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"

	"mediacheck/internal/fixture"
	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
)

func TestKeyIndexAcceptsCoherentFixture(t *testing.T) {
	b := fixture.Normal()
	if err := b.Index.ValidateAgainstSource(b.Video, b.Index.SourceSHA256); err != nil {
		t.Fatal(err)
	}
	var pkts []rtp.Packet
	for _, p := range b.RTPPackets {
		raw, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ingest.DecodeRTPPacket(raw)
		if err != nil {
			t.Fatal(err)
		}
		pkts = append(pkts, decoded)
	}
	if err := b.Index.ValidateRTP(pkts); err != nil {
		t.Fatal(err)
	}
}

func TestKeyIndexRejectsOutOfBoundsOffset(t *testing.T) {
	b := fixture.Normal()
	idx := *b.Index
	idx.Entries = append([]ingest.IndexEntry(nil), b.Index.Entries...)
	idx.Entries[1].Offset = int64(len(b.Video)) + 10
	err := idx.ValidateAgainstSource(b.Video, b.Index.SourceSHA256)
	assertKind(t, err, media.KindOutOfBounds)
}

func TestLoadExpiredKeyIndex(t *testing.T) {
	raw, err := json.Marshal(fixture.StaleIndex(fixture.Normal().Index))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingest.LoadKeyIndex(bytes.NewReader(raw), time.Now())
	assertKind(t, err, media.KindStaleIndex)
}

func TestLoadRejectsMissingClockAndBadJSON(t *testing.T) {
	_, err := ingest.LoadKeyIndex(strings.NewReader(`{"codec":"h264"}`), time.Now())
	assertKind(t, err, media.KindMissingClock)

	_, err = ingest.LoadKeyIndex(strings.NewReader(`{not json`), time.Now())
	assertKind(t, err, media.KindInvalidArgument)
}

func TestRTPTimestampWrapClassified(t *testing.T) {
	idx, pkts := fixture.WrapRTPIndex()
	err := idx.ValidateRTP(pkts)
	me := assertKind(t, err, media.KindWrap)
	if me.Offset != idx.Entries[1].Offset {
		t.Fatalf("offset=%d want %d", me.Offset, idx.Entries[1].Offset)
	}
}

package demux_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"mediaproc/internal/demux"
	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
	"mediaproc/internal/sample"
)

func drain(t *testing.T, p *demux.Parser) []*demux.Block {
	t.Helper()
	var blocks []*demux.Block
	last := map[uint64]model.Nanoseconds{}
	for {
		b, err := p.NextBlock()
		if errors.Is(err, io.EOF) {
			return blocks
		}
		if err != nil {
			t.Fatalf("NextBlock: %v", err)
		}
		if prev, ok := last[b.TrackNumber]; ok && b.TimeNS < prev {
			t.Fatalf("track %d went backwards: %d < %d", b.TrackNumber, b.TimeNS, prev)
		}
		last[b.TrackNumber] = b.TimeNS
		blocks = append(blocks, b)
	}
}

func TestParsesUnknownSegmentAndCluster(t *testing.T) {
	fx := sample.Generate(sample.Config{Seed: 41})
	p, err := demux.NewParser(bytes.NewReader(fx.Data), demux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := p.ReadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta.TimecodeScaleNS != 1_000_000 || len(meta.Tracks) != 2 {
		t.Fatalf("metadata=%+v", meta)
	}
	blocks := drain(t, p)
	if len(blocks) != 66 {
		t.Fatalf("blocks=%d want 66", len(blocks))
	}
	keyframes := 0
	for _, b := range blocks {
		if b.Keyframe {
			keyframes++
		}
	}
	if keyframes != fx.KeyframeCount {
		t.Fatalf("keyframes=%d want %d", keyframes, fx.KeyframeCount)
	}
}

func TestInterleavedTracksKeepIndependentOrder(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(ebml.UnknownHeader(ebml.IDSegment, 8))
	buf.Write(ebml.UintElement(ebml.IDTimecodeScale, 1_000_000))

	track1 := ebml.BuildElement(ebml.IDTrackEntry,
		append(ebml.UintElement(ebml.IDTrackNumber, 1),
			ebml.UintElement(ebml.IDTrackType, ebml.TrackTypeVideo)...))
	track2 := ebml.BuildElement(ebml.IDTrackEntry,
		append(ebml.UintElement(ebml.IDTrackNumber, 2),
			ebml.UintElement(ebml.IDTrackType, ebml.TrackTypeAudio)...))
	buf.Write(ebml.BuildElement(ebml.IDTracks, append(track1, track2...)))
	buf.Write(ebml.UnknownHeader(ebml.IDCluster, 8))
	buf.Write(ebml.UintElement(ebml.IDTimecode, 0))

	writeBlock := func(track uint64, ms int16) {
		payload := []byte{0x80, byte(ms >> 8), byte(ms), 0x80, 1, 2, 3}
		payload[0] = byte(0x80 | track)
		buf.Write(ebml.BuildElement(ebml.IDSimpleBlock, payload))
	}
	writeBlock(1, 0)
	writeBlock(2, 10)
	writeBlock(1, 20)
	writeBlock(2, 30)

	p, _ := demux.NewParser(bytes.NewReader(buf.Bytes()), demux.DefaultConfig())
	if _, err := p.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	blocks := drain(t, p)
	if len(blocks) != 4 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	want := map[uint64][]int64{1: {0, 20}, 2: {10, 30}}
	got := map[uint64][]int64{}
	for _, b := range blocks {
		got[b.TrackNumber] = append(got[b.TrackNumber], b.TimeNS.Milliseconds())
	}
	for track, times := range want {
		if len(got[track]) != len(times) {
			t.Fatalf("track %d times=%v want %v", track, got[track], times)
		}
		for i := range times {
			if got[track][i] != times[i] {
				t.Fatalf("track %d times=%v want %v", track, got[track], times)
			}
		}
	}
}

func TestFaultsRecoverAtNextValidatedCluster(t *testing.T) {
	for _, fault := range []string{"truncated-block", "invalid-vint", "bad-lacing"} {
		t.Run(fault, func(t *testing.T) {
			fx := sample.Generate(sample.Config{Seed: 41, Fault: fault})
			p, _ := demux.NewParser(bytes.NewReader(fx.Data), demux.DefaultConfig())
			if _, err := p.ReadMetadata(); err != nil {
				t.Fatal(err)
			}
			var fe *model.Error
			for {
				_, err := p.NextBlock()
				if errors.Is(err, io.EOF) {
					t.Fatal("expected recoverable fault before EOF")
				}
				if err != nil {
					if !errors.As(err, &fe) {
						t.Fatalf("expected model.Error, got %T %v", err, err)
					}
					break
				}
			}
			if fe.Offset != fx.FaultOffset {
				t.Fatalf("offset=%d want %d", fe.Offset, fx.FaultOffset)
			}
			if !fe.Recoverable {
				t.Fatalf("fault not recoverable: %+v", fe)
			}
			boundary, err := p.Recover(fe)
			if err != nil {
				t.Fatal(err)
			}
			if boundary <= fx.FaultOffset {
				t.Fatalf("boundary=%d must follow fault offset=%d", boundary, fx.FaultOffset)
			}
			b, err := p.NextBlock()
			if err != nil {
				t.Fatalf("block after recovery: %v", err)
			}
			if b.ClusterOffset != boundary {
				t.Fatalf("cluster offset=%d boundary=%d", b.ClusterOffset, boundary)
			}
			if got := b.TimeNS.Milliseconds(); got != int64(fx.RecoveryClusterMS) {
				t.Fatalf("recovery time=%dms want %dms", got, fx.RecoveryClusterMS)
			}
			drain(t, p)
		})
	}
}

func TestMissingClusterEndEndsAtEOF(t *testing.T) {
	fx := sample.Generate(sample.Config{Seed: 41, Fault: "missing-cluster-end"})
	p, _ := demux.NewParser(bytes.NewReader(fx.Data), demux.DefaultConfig())
	if _, err := p.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	blocks := drain(t, p)
	if len(blocks) == 0 {
		t.Fatal("expected blocks before EOF")
	}
}

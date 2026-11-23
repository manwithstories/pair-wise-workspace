package fixture

import (
	"bytes"
	"testing"
	"time"

	"mediakernel/internal/matroska"
	"mediakernel/internal/ebml"
)

func testParams() Params {
	return Params{Seed: 17, Duration: 2 * time.Second, BitrateKbps: 420, LossBurst: 12}
}

func TestGenerateDeterministic(t *testing.T) {
	m1 := Generate(testParams())
	m2 := Generate(testParams())
	if !bytes.Equal(m1.WebM, m2.WebM) {
		t.Fatal("WebM not deterministic")
	}
	if m1.DurationNs != int64(2*time.Second) {
		t.Fatalf("duration=%d", m1.DurationNs)
	}
	// 60 视频帧 + 100 音频帧
	if len(m1.Blocks) != 160 {
		t.Fatalf("blocks=%d", len(m1.Blocks))
	}
}

func TestGenerateParsesBack(t *testing.T) {
	m := Generate(testParams())
	var blocks []matroska.Block
	var problems []error
	ev := evFunc{
		block: func(b matroska.Block) { blocks = append(blocks, b) },
		prob:  func(err error) { problems = append(problems, err) },
	}
	if err := matroska.Parse(ebml.NewReader(bytes.NewReader(m.WebM)), ev); err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(blocks) != len(m.Blocks) {
		t.Fatalf("parsed %d blocks, want %d", len(blocks), len(m.Blocks))
	}
	for i, blk := range m.Blocks {
		got := blocks[i]
		if got.Track != blk.Track || int64(got.Abs) != blk.AbsNs ||
			got.Keyframe != blk.Keyframe || !bytes.Equal(got.Data, blk.Data) {
			t.Fatalf("block %d mismatch", i)
		}
	}
}

type evFunc struct {
	block func(matroska.Block)
	prob  func(error)
}

func (e evFunc) Tracks([]matroska.Track) {}
func (e evFunc) Block(b matroska.Block)  { e.block(b) }
func (e evFunc) Problem(err error)       { e.prob(err) }

func TestPacketize(t *testing.T) {
	m := Generate(testParams())
	p1 := Packetize(m.Blocks, testParams())
	p2 := Packetize(m.Blocks, testParams())
	if len(p1) != len(p2) || len(p1) == 0 {
		t.Fatalf("packets=%d", len(p1))
	}
	for i := range p1 {
		if p1[i].Arrival != p2[i].Arrival || p1[i].Lost != p2[i].Lost {
			t.Fatalf("trace not deterministic at %d", i)
		}
		if p1[i].Arrival < p1[i].Send {
			t.Fatalf("arrival before send at %d", i)
		}
	}
	// 突发丢包数量
	lost := 0
	for _, pkt := range p1 {
		if pkt.Lost {
			lost++
		}
	}
	if lost != 12 {
		t.Fatalf("lost=%d want 12", lost)
	}
	// 每轨序号连续
	seqs := map[uint64][]uint16{}
	for _, pkt := range p1 {
		seqs[pkt.Track] = append(seqs[pkt.Track], pkt.RTP.SequenceNumber)
	}
	for track, ss := range seqs {
		for i := 1; i < len(ss); i++ {
			if ss[i]-ss[i-1] != 1 {
				t.Fatalf("track %d seq gap at %d", track, i)
			}
		}
	}
}

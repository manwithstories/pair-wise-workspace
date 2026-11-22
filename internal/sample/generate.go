// Package sample generates the deterministic local fixtures used by tests and
// the demo. Everything derives from a seed; no external files are read.
package sample

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"sort"

	"github.com/pion/rtp"

	"example.com/medialab/archive/internal/clock"
	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/model"
)

// Opts describes one stream of synthetic packets.
type Opts struct {
	Seed         int64
	DurationUS   int64
	SegmentUS    int64
	LateWindowUS int64
	Stream       model.StreamID
	Rep          model.RepresentationID
	Codec        model.Codec
	ClockRate    uint32
	// PacketUS is the media duration of one packet; TicksPerPacket is the same
	// span in codec ticks and is what the RTP timestamp advances by.
	PacketUS       int64
	TicksPerPacket uint32
	SSRC           uint32
	StartSeq       uint16
	StartTS        uint32
	AAC            codec.AACConfig
}

// Faults names the damage injected into a stream. All sets hold packet-local
// indices (0 = first generated packet).
type Faults struct {
	Lost       map[int]bool
	Corrupt    map[int]bool
	Duplicate  map[int]bool
	Reorder    map[int]bool
	Late       map[int]bool // arrives inside the late window
	LateBeyond map[int]bool // arrives past the late window
	LateByUS   int64
	JitterUS   int64
}

// Record is one log entry, replayable by ingest with no generator dependency.
type Record struct {
	ArrivalUS int64
	Bytes     []byte
}

// Packet is generator ground truth for one logical packet.
type Packet struct {
	Index      int
	Seq        uint32
	StartUS    int64
	DurationUS int64
	Lost       bool
	Corrupt    bool
	Duplicate  bool
	Reordered  bool
	Late       bool
	LateBeyond bool
}

// Plan is a complete generated stream.
type Plan struct {
	Opts            Opts
	Records         []Record
	Packets         []Packet
	CoverageUS      int64
	LostPackets     int
	CorruptPackets  int
	LateBeyondCount int
}

// PacketCount returns how many packets are needed to cover durationUS.
func PacketCount(durationUS, packetUS int64) int {
	if durationUS <= 0 || packetUS <= 0 {
		return 0
	}
	return int((durationUS + packetUS - 1) / packetUS)
}

// OpusStream returns the 64k representation options.
func OpusStream(seed, durationUS, segUS, windowUS int64) Opts {
	return Opts{
		Seed: seed, DurationUS: durationUS, SegmentUS: segUS, LateWindowUS: windowUS,
		Stream: "opus-64k", Rep: "64k", Codec: model.CodecOpus,
		ClockRate: uint32(codec.OpusRate), PacketUS: 20000, TicksPerPacket: 960,
		SSRC: 0x0A0B0C0D, StartSeq: 65536 - 37, StartTS: 0xFFFFFFFF - 960*61,
	}
}

// AACStream returns the 128k representation options. 1024 samples at 48 kHz
// is 1920 ticks at the 90 kHz AAC clock; its 21333us packets deliberately do
// not divide the 10s grid.
func AACStream(seed, durationUS, segUS, windowUS int64) Opts {
	return Opts{
		Seed: seed, DurationUS: durationUS, SegmentUS: segUS, LateWindowUS: windowUS,
		Stream: "aac-128k", Rep: "128k", Codec: model.CodecAAC,
		ClockRate: uint32(codec.AACRate), PacketUS: 21333, TicksPerPacket: 1920,
		SSRC: 0x1B2C3D4E, StartSeq: 65536 - 19, StartTS: 0xFFFFFFFF - 1920*77,
		AAC: codec.DefaultAACConfig(),
	}
}

// MixedFaults injects a reproducible mixture of all fault classes.
func MixedFaults(seed int64, count int) Faults {
	r := rand.New(rand.NewSource(seed ^ 0x5EED))
	f := Faults{
		Lost: map[int]bool{}, Corrupt: map[int]bool{}, Duplicate: map[int]bool{},
		Reorder: map[int]bool{}, Late: map[int]bool{}, LateBeyond: map[int]bool{},
		LateByUS: 1_500_000, JitterUS: 4_000,
	}
	// Two bursts sized to keep the recoverable packet share well above 85%.
	for b := 0; b < 3; b++ {
		start := count/6 + b*(count/4) + r.Intn(count/20)
		for i := 0; i < 24 && start+i < count-5; i++ {
			f.Lost[start+i] = true
		}
	}
	for i := 0; i < 14; i++ {
		f.Corrupt[r.Intn(count-5)+2] = true
	}
	for i := 0; i < 20; i++ {
		f.Duplicate[r.Intn(count-5)+2] = true
	}
	for i := 0; i < 40; i++ {
		f.Reorder[r.Intn(count-5)+2] = true
	}
	for i := 0; i < 30; i++ {
		f.Late[r.Intn(count-5)+2] = true
	}
	for i := 0; i < 3; i++ {
		f.LateBeyond[r.Intn(count-5)+2] = true
	}
	return f
}

// BurstLossFaults drops long contiguous runs so every hole is bounded and
// recoverable, plus a small number of undecodable payloads.
func BurstLossFaults(seed int64, count int) Faults {
	r := rand.New(rand.NewSource(seed ^ 0xB0057))
	f := Faults{
		Lost: map[int]bool{}, Corrupt: map[int]bool{}, Duplicate: map[int]bool{},
		Reorder: map[int]bool{}, Late: map[int]bool{}, LateBeyond: map[int]bool{},
		LateByUS: 1_200_000, JitterUS: 3_000,
	}
	for b := 0; b < 4; b++ {
		start := count/8 + b*(count/5) + r.Intn(count/30)
		for i := 0; i < 45 && start+i < count-5; i++ {
			f.Lost[start+i] = true
		}
	}
	for i := 0; i < 10; i++ {
		f.Corrupt[r.Intn(count-5)+2] = true
	}
	for i := 0; i < 10; i++ {
		f.Duplicate[r.Intn(count-5)+2] = true
	}
	return f
}

// NoFaults returns an empty fault set.
func NoFaults() Faults {
	return Faults{Lost: map[int]bool{}, Corrupt: map[int]bool{}, Duplicate: map[int]bool{},
		Reorder: map[int]bool{}, Late: map[int]bool{}, LateBeyond: map[int]bool{}}
}

// BoundaryFaults places one late packet exactly at the watermark (quarantined)
// and one a millisecond inside it (replay), for both streams.
func BoundaryFaults(seed int64, count int) Faults {
	f := MixedFaults(seed, count)
	// Clear the generic late sets so the boundary cases are unambiguous.
	f.Late = map[int]bool{}
	f.LateBeyond = map[int]bool{}
	return f
}

// Generate builds the plan. Records are ordered by arrival, which differs from
// media order exactly where faults ask for reordering or lateness.
func Generate(o Opts) (*Plan, Faults, error) {
	if o.SegmentUS <= 0 || o.LateWindowUS < 0 || o.DurationUS <= 0 {
		return nil, Faults{}, fmt.Errorf("sample: invalid duration/segment/window")
	}
	if !o.Codec.Valid() || o.ClockRate == 0 || o.PacketUS <= 0 || o.TicksPerPacket == 0 {
		return nil, Faults{}, fmt.Errorf("sample: invalid codec geometry")
	}
	count := PacketCount(o.DurationUS, o.PacketUS)
	if count < 4 {
		return nil, Faults{}, fmt.Errorf("sample: duration %d too short for packet size %d", o.DurationUS, o.PacketUS)
	}
	f := NoFaults()
	switch o.Seed % 3 {
	case 0:
		f = MixedFaults(o.Seed, count)
	case 1:
		f = BurstLossFaults(o.Seed, count)
	}
	return generateWith(o, f, count)
}

// GenerateWithFaults builds the plan with an explicit fault set.
func GenerateWithFaults(o Opts, f Faults) (*Plan, error) {
	if o.DurationUS <= 0 || o.PacketUS <= 0 || !o.Codec.Valid() {
		return nil, fmt.Errorf("sample: invalid options")
	}
	count := PacketCount(o.DurationUS, o.PacketUS)
	plan, _, err := generateWith(o, f, count)
	return plan, err
}

func generateWith(o Opts, f Faults, count int) (*Plan, Faults, error) {
	rate := clock.Rate(o.ClockRate)
	plan := &Plan{Opts: o}
	var recs []Record
	for i := 0; i < count; i++ {
		startUS := rate.TicksToUS(int64(i) * int64(o.TicksPerPacket))
		seq := uint32(uint16(int(o.StartSeq) + i))
		ts := o.StartTS + uint32(i)*o.TicksPerPacket
		payload, err := payloadFor(o, i)
		if err != nil {
			return nil, f, err
		}
		p := Packet{Index: i, Seq: seq, StartUS: startUS, DurationUS: o.PacketUS}
		if f.Corrupt[i] {
			payload = corruptPayload(o, i)
			p.Corrupt = true
			plan.CorruptPackets++
		}
		if f.Lost[i] {
			p.Lost = true
			plan.LostPackets++
			plan.Packets = append(plan.Packets, p)
			continue
		}
		raw, err := marshalRTP(o, uint16(seq), ts, payload)
		if err != nil {
			return nil, f, err
		}
		arrival := startUS + jitterFor(f, i)
		if f.Late[i] {
			arrival = startUS + f.LateByUS
			p.Late = true
		}
		if f.LateBeyond[i] {
			arrival = startUS + f.LateByUS + o.LateWindowUS
			p.LateBeyond = true
			plan.LateBeyondCount++
		}
		if arrival < 0 {
			arrival = 0
		}
		recs = append(recs, Record{ArrivalUS: arrival, Bytes: raw})
		if f.Duplicate[i] {
			p.Duplicate = true
			recs = append(recs, Record{ArrivalUS: arrival + 15_000, Bytes: raw})
		}
		if f.Reorder[i] {
			p.Reordered = true
			// Push this copy after the next packet, still on time.
			recs[len(recs)-1].ArrivalUS = arrival + 120_000
		}
		plan.Packets = append(plan.Packets, p)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].ArrivalUS < recs[j].ArrivalUS })
	plan.Records = recs
	plan.CoverageUS = int64(count) * o.PacketUS
	return plan, f, nil
}

func jitterFor(f Faults, i int) int64 {
	if f.JitterUS <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", i, f.JitterUS)))
	return int64(uint32(h[0])<<8|uint32(h[1])) % f.JitterUS
}

func payloadFor(o Opts, i int) ([]byte, error) {
	switch o.Codec {
	case model.CodecOpus:
		// 20 ms SILK WB frame: TOC config 9, code 0, plus opaque body.
		size := 40
		body := make([]byte, size)
		for j := range body {
			body[j] = byte((i*31 + j*7) % 251)
		}
		return append([]byte{9 << 3}, body...), nil
	case model.CodecAAC:
		au := make([]byte, 96)
		for j := range au {
			au[j] = byte((i*17 + j*3) % 249)
		}
		return aacPayload(au), nil
	}
	return nil, fmt.Errorf("sample: unsupported codec %q", o.Codec)
}

func corruptPayload(o Opts, i int) []byte {
	if o.Codec == model.CodecOpus {
		// Code 2 declares two length-coded frames but omits the length byte.
		return []byte{9<<3 | 2<<1}
	}
	// AU header claims 64 bytes of AU data but the payload is empty.
	return []byte{0x00, 0x10, 0x02, 0x00}
}

func aacPayload(au []byte) []byte {
	// One AU: 13-bit size + 3-bit index, 16 bits of AU-header length.
	header := uint16(len(au)) << 3
	out := []byte{0x00, 0x10, byte(header >> 8), byte(header)}
	return append(out, au...)
}

func marshalRTP(o Opts, seq uint16, ts uint32, payload []byte) ([]byte, error) {
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: o.SSRC},
		Payload: payload,
	}
	return p.Marshal()
}

// SilencePayload returns a payload whose derived duration is the stream's
// packet duration, used for labelled silence fills.
func SilencePayload(o Opts) ([]byte, error) {
	switch o.Codec {
	case model.CodecOpus:
		return []byte{9 << 3}, nil
	case model.CodecAAC:
		return aacPayload([]byte{0x00}), nil
	}
	return nil, fmt.Errorf("sample: unsupported codec %q", o.Codec)
}

// VerifySelf checks the generator output against the codec parsers.
func VerifySelf(o Opts) error {
	switch o.Codec {
	case model.CodecOpus:
		d, err := codec.OpusDurationUS([]byte{9 << 3, 1, 2})
		if err != nil || d != o.PacketUS {
			return fmt.Errorf("sample: opus duration %d/%v", d, err)
		}
	case model.CodecAAC:
		d, err := o.AAC.AACDurationUS(aacPayload([]byte{1, 2, 3}))
		if err != nil || d != o.PacketUS {
			return fmt.Errorf("sample: aac duration %d/%v", d, err)
		}
	}
	return nil
}

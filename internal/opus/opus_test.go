package opus

import (
	"encoding/binary"
	"math"
	"testing"
)

// tone fills interleaved int16 stereo with a deterministic sine wave.
func tone(frame, total int, freq float64) []int16 {
	pcm := make([]int16, frame*2)
	for i := 0; i < frame; i++ {
		t := float64(total+i) / 48000
		v := int16(6000 * math.Sin(2*math.Pi*freq*t))
		pcm[2*i], pcm[2*i+1] = v, v
	}
	return pcm
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	enc, err := NewEncoder(48000, 2, 64000, ApplicationVoIP, true, 20)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	defer enc.Close()
	dec, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer dec.Close()
	if got := enc.Lookahead(); got < 0 || got > 4800 {
		t.Fatalf("lookahead = %d, outside expected range", got)
	}
	var packets [][]byte
	for i := 0; i < 10; i++ {
		data, err := enc.Encode(tone(960, i*960, 440), 960)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("empty packet")
		}
		packets = append(packets, data)
	}
	var total int
	for _, p := range packets {
		pcm, err := dec.Decode(p, 960, false)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		total += len(pcm) / 2
	}
	if total != 9600 {
		t.Fatalf("decoded %d samples, want 9600", total)
	}
}

func TestInBandFECProducesAudioForMissingSlot(t *testing.T) {
	enc, err := NewEncoder(48000, 2, 24000, ApplicationVoIP, true, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var packets [][]byte
	for i := 0; i < 8; i++ {
		p, err := enc.Encode(tone(960, i*960, 300), 960)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, p)
	}
	dec, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i := 0; i < 3; i++ {
		if _, err := dec.Decode(packets[i], 960, false); err != nil {
			t.Fatal(err)
		}
	}
	// Recover slot 3 from the FEC data in packet 4.
	fec, err := dec.Decode(packets[4], 960, true)
	if err != nil {
		t.Fatalf("FEC decode: %v", err)
	}
	if len(fec) != 1920 {
		t.Fatalf("FEC produced %d values, want 1920", len(fec))
	}
	energy := 0.0
	for _, v := range fec {
		energy += float64(v) * float64(v)
	}
	if energy == 0 {
		t.Fatal("FEC output is pure silence")
	}
}

func TestPLCProducesAudioForMissingSlot(t *testing.T) {
	enc, err := NewEncoder(48000, 2, 24000, ApplicationAudio, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i := 0; i < 4; i++ {
		p, err := enc.Encode(tone(960, i*960, 300), 960)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Decode(p, 960, false); err != nil {
			t.Fatal(err)
		}
	}
	pcm, err := dec.Decode(nil, 960, false)
	if err != nil {
		t.Fatalf("PLC: %v", err)
	}
	if len(pcm) != 1920 {
		t.Fatalf("PLC produced %d values, want 1920", len(pcm))
	}
}

func TestOpusHeadLayout(t *testing.T) {
	h := OpusHead(2, 312, 48000)
	if len(h) != 19 {
		t.Fatalf("head length = %d, want 19", len(h))
	}
	if string(h[:8]) != "OpusHead" {
		t.Fatalf("magic = %q", h[:8])
	}
	if h[8] != 1 || h[9] != 2 {
		t.Fatalf("version/channels = %d/%d", h[8], h[9])
	}
	if got := binary.LittleEndian.Uint16(h[10:12]); got != 312 {
		t.Fatalf("pre-skip = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[12:16]); got != 48000 {
		t.Fatalf("input rate = %d", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	e, err := NewEncoder(48000, 1, 32000, ApplicationVoIP, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e.Close()
	if _, err := e.Encode(make([]int16, 960), 960); err != ErrClosed {
		t.Fatalf("Encode after Close = %v, want ErrClosed", err)
	}
	d, err := NewDecoder(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Close()
	if _, err := d.Decode(nil, 960, false); err != ErrClosed {
		t.Fatalf("Decode after Close = %v, want ErrClosed", err)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	if _, err := NewEncoder(48000, 3, 64000, ApplicationVoIP, false, 0); err == nil {
		t.Fatal("expected error for 3 channels")
	}
	if _, err := NewEncoder(1000, 2, 64000, ApplicationVoIP, false, 0); err == nil {
		t.Fatal("expected error for unsupported rate")
	}
	d, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Decode(nil, MaxFrameSamples+1, false); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

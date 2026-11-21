package rtpprobe

import (
	"bytes"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

func TestValidateRoundTripsPayloads(t *testing.T) {
	samples := [][]byte{
		bytes.Repeat([]byte{0x11}, 100),
		bytes.Repeat([]byte{0x22}, 1199),
		bytes.Repeat([]byte{0x33}, 1200),
		bytes.Repeat([]byte{0x44}, 2500),
	}
	res, err := Validate(samples, 96, 48000, DefaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 4 {
		t.Fatalf("samples=%d", res.Samples)
	}
	// 1 + 1 + 1 + 3 packets at 1200 bytes.
	if res.Packets != 6 {
		t.Fatalf("packets=%d", res.Packets)
	}
	if res.MaxPayload > DefaultMTU {
		t.Fatalf("payload %d exceeds mtu", res.MaxPayload)
	}
}

func TestValidateRejectsEmptySample(t *testing.T) {
	_, err := Validate([][]byte{nil}, 96, 48000, 0)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if fault.StageOf(err) != fault.StageVerify {
		t.Fatalf("stage=%q", fault.StageOf(err))
	}
}

func TestValidateNoSamplesIsClean(t *testing.T) {
	res, err := Validate(nil, 96, 48000, 0)
	if err != nil || res.Packets != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestValidateLargeSampleSplits(t *testing.T) {
	sample := make([]byte, 5000)
	for i := range sample {
		sample[i] = byte(i)
	}
	res, err := Validate([][]byte{sample}, 96, 90000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Packets != 5 || res.MaxPayload != 1000 {
		t.Fatalf("res=%+v", res)
	}
}

func TestValidateAcceptsMultiPacketFrame(t *testing.T) {
	// A frame larger than the MTU must split cleanly; the marker bit only
	// appears on the final packet.
	sample := bytes.Repeat([]byte("x"), DefaultMTU+1)
	res, err := Validate([][]byte{sample}, 111, 44100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Packets != 2 {
		t.Fatalf("packets=%d", res.Packets)
	}
}

func TestValidatorIncrementalMatchesBatch(t *testing.T) {
	samples := [][]byte{
		bytes.Repeat([]byte{1}, 2000),
		bytes.Repeat([]byte{2}, 500),
		bytes.Repeat([]byte{3}, 1200),
	}
	batch, err := Validate(samples, 96, 48000, DefaultMTU)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(96, 48000, DefaultMTU)
	for i, s := range samples {
		if err := v.AddSample(s, i); err != nil {
			t.Fatal(err)
		}
	}
	if got := v.Result(); got != batch {
		t.Fatalf("incremental=%+v batch=%+v", got, batch)
	}
}

func TestValidatorRejectsEmptySample(t *testing.T) {
	v := NewValidator(96, 48000, 0)
	if err := v.AddSample(nil, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

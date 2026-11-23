package index

import (
	"encoding/binary"
	"testing"
)

// TestSampleHeaderParses checks the structural verifier accepts a realistic
// length-prefixed sample and rejects corrupted ones. This covers the
// "parseable offset" requirement directly.
func TestSampleHeaderParses(t *testing.T) {
	const size = 64
	good := make([]byte, size)
	binary.BigEndian.PutUint32(good[0:4], size-4)
	good[4] = 0x65 // plausible NAL header
	if err := parseSampleHeader(good, size, FormatFinding); err != nil {
		t.Fatalf("valid sample rejected: %v", err)
	}

	// Declared NAL length overshoots the sample size.
	bad := make([]byte, size)
	binary.BigEndian.PutUint32(bad[0:4], size)
	if err := parseSampleHeader(bad, size, FormatFinding); err == nil {
		t.Fatalf("corrupt sample accepted")
	}

	// Forbidden zero bit set in the NAL header.
	binary.BigEndian.PutUint32(bad[0:4], 8)
	bad[4] = 0x80
	if err := parseSampleHeader(bad, size, FormatFinding); err == nil {
		t.Fatalf("invalid NAL header accepted")
	}

	// Too short, zero-length, and opaque-format cases.
	if err := parseSampleHeader([]byte{1, 2}, 2, FormatFinding); err == nil {
		t.Fatalf("short sample accepted")
	}
	zero := make([]byte, size)
	if err := parseSampleHeader(zero, size, FormatFinding); err == nil {
		t.Fatalf("zero-length NAL accepted")
	}
	if err := parseSampleHeader(make([]byte, size), size, FormatOpaque); err != nil {
		t.Fatalf("opaque sample rejected: %v", err)
	}
}

// TestOpaqueFormatAcceptsUnstructuredSamples proves the opaque verifier does
// not impose NAL semantics on codecs that do not use them.
func TestOpaqueFormatAcceptsUnstructuredSamples(t *testing.T) {
	path := buildTestMP4(t, 80)
	idx, err := BuildFileWithFormat(path, FormatOpaque)
	if err != nil {
		t.Fatalf("index opaque: %v", err)
	}
	if idx.SampleFormat != "opaque" {
		t.Fatalf("format = %q", idx.SampleFormat)
	}
	if idx.Roundtrip != 1.0 {
		t.Fatalf("roundtrip = %v", idx.Roundtrip)
	}
}

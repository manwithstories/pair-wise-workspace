package wavio

import (
	"bytes"
	"testing"
)

func buildWAVWithChunks(body []byte, extraChunks ...[]byte) []byte {
	var out bytes.Buffer
	fmtSize := uint32(16)
	riffSize := uint32(4) + 8 + fmtSize
	for _, chunk := range extraChunks {
		riffSize += uint32(len(chunk))
	}
	riffSize += 8 + uint32(len(body))
	out.WriteString("RIFF")
	writeU32(&out, riffSize)
	out.WriteString("WAVE")
	out.WriteString("fmt ")
	writeU32(&out, fmtSize)
	writeU16(&out, FormatPCM)
	writeU16(&out, 1)
	writeU32(&out, 48000)
	writeU32(&out, 48000*2)
	writeU16(&out, 2)
	writeU16(&out, 16)
	for _, chunk := range extraChunks {
		out.Write(chunk)
	}
	out.WriteString("data")
	writeU32(&out, uint32(len(body)))
	out.Write(body)
	return out.Bytes()
}

func writeU32(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v)); b.WriteByte(byte(v >> 8)); b.WriteByte(byte(v >> 16)); b.WriteByte(byte(v >> 24))
}
func writeU16(b *bytes.Buffer, v uint16) {
	b.WriteByte(byte(v)); b.WriteByte(byte(v >> 8))
}

func TestUnknownAndPaddedChunksSkipped(t *testing.T) {
	body := make([]byte, 48000*2*2)
	oddChunk := []byte("JUNK")
	oddChunk = append(oddChunk, 5, 0, 0, 0)
	oddChunk = append(oddChunk, 1, 2, 3, 4, 5, 0)
	unknown := []byte("INFO")
	unknown = append(unknown, 4, 0, 0, 0)
	unknown = append(unknown, 9, 8, 7, 6)
	data := buildWAVWithChunks(body, oddChunk, unknown)
	reader, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().DataOffset == 0 {
		t.Fatal("data offset not located after skipped chunks")
	}
	frames, err := reader.ReadFrames(1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || len(frames[0]) != 1024 {
		t.Fatalf("unexpected frame shape: %d channels", len(frames))
	}
}

func TestTruncatedCarriesOffsetAndLength(t *testing.T) {
	full := make([]byte, 48000*2*2)
	data := buildWAVWithChunks(full)
	cut := data[:len(data)-1000]
	reader, err := NewReader(bytes.NewReader(cut))
	if err != nil {
		t.Fatal(err)
	}
	var truncated *TruncatedDataError
	for {
		_, readErr := reader.ReadFrames(4096)
		if readErr == nil {
			continue
		}
		if !IsTruncated(readErr) {
			t.Fatalf("expected truncated error, got %v", readErr)
		}
		truncated, _ = readErr.(*TruncatedDataError)
		break
	}
	if truncated.Declared != int64(len(full)) {
		t.Fatalf("declared %d want %d", truncated.Declared, len(full))
	}
	if truncated.ActuallyGot >= truncated.Declared {
		t.Fatalf("actually got %d should be below declared %d", truncated.ActuallyGot, truncated.Declared)
	}
	if truncated.DataOffset != reader.Header().DataOffset {
		t.Fatal("truncated offset must equal data payload offset")
	}
}

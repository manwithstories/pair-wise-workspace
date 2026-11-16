package box

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildTiny assembles a minimal in-memory file: ftyp + moov(trak) + uuid.
func buildTiny(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	box := func(typ string, payload []byte) {
		binary.Write(&buf, binary.BigEndian, uint32(8+len(payload)))
		buf.WriteString(typ)
		buf.Write(payload)
	}
	box("ftyp", []byte("isom00000000"))
	var moov bytes.Buffer
	binary.Write(&moov, binary.BigEndian, uint32(8))
	moov.WriteString("trak")
	box("moov", moov.Bytes())
	// uuid box: 16-byte usertype + 4-byte payload
	u := make([]byte, 16+4)
	for i := range u {
		u[i] = byte(i)
	}
	box("uuid", u)
	return buf.Bytes()
}

func TestParseTinyTree(t *testing.T) {
	data := buildTiny(t)
	f, err := Parse(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Count() != 4 { // ftyp, moov, trak, uuid
		t.Fatalf("Count = %d, want 4", f.Count())
	}
	moov := f.Find("moov")
	if moov == nil || moov.Find("trak") == nil {
		t.Fatal("moov/trak not found")
	}
	uu := f.Find("uuid")
	if uu == nil || uu.UUID == nil {
		t.Fatal("uuid box lost")
	}
	if uu.UUID[15] != 15 || uu.DataSize() != 4 {
		t.Fatalf("uuid usertype/payload wrong: %+v", uu)
	}
}

func TestParseTruncated(t *testing.T) {
	data := buildTiny(t)
	// Cut inside the moov payload (ftyp is 20 bytes, moov follows).
	if _, err := Parse(bytes.NewReader(data[:25]), 25, nil); err == nil {
		t.Fatal("expected error for truncated file")
	} else if !IsStructure(err) {
		t.Fatalf("want StructureError, got %T: %v", err, err)
	}
}

func TestParseOverrunningBox(t *testing.T) {
	// A box whose declared size overruns the file must be rejected.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(1<<20))
	buf.WriteString("free")
	data := buf.Bytes()
	if _, err := Parse(bytes.NewReader(data), int64(len(data)), nil); err == nil {
		t.Fatal("expected overrun error")
	} else if !IsStructure(err) {
		t.Fatalf("want StructureError, got %v", err)
	}
}

func TestParseDepthLimit(t *testing.T) {
	lim := &Limits{MaxDepth: 0} // forbid any container recursion
	data := buildTiny(t)
	if _, err := Parse(bytes.NewReader(data), int64(len(data)), lim); err == nil {
		t.Fatal("expected depth limit error")
	}
}

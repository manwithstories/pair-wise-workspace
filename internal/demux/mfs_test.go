package demux

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"relay/internal/media"
)

// writeMFS 拼接若干完整帧为 MFS/1 字节流。
func writeMFS(frames ...*media.Frame) []byte {
	var buf bytes.Buffer
	for _, f := range frames {
		rec := media.EncodeRecord(f)
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], uint32(len(rec)))
		buf.Write(p[:])
		buf.Write(rec)
	}
	return buf.Bytes()
}

func sampleFrame(cam uint16, seq uint64, payload string) *media.Frame {
	return &media.Frame{
		Version: media.WireVersion1, Codec: media.CodecPNG,
		CameraID: cam, Sequence: seq, EventNS: media.TimeNS(seq) * media.Second,
		Width: 16, Height: 16, Payload: []byte(payload),
	}
}

func TestMFSRoundTrip(t *testing.T) {
	in := writeMFS(sampleFrame(1, 1, "a"), sampleFrame(2, 7, "bb"), sampleFrame(1, 2, "ccc"))
	frames, err := ReadAll(bytes.NewReader(in), "test.mfs")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if frames[1].CameraID != 2 || frames[1].Sequence != 7 ||
		string(frames[1].Payload) != "bb" || frames[1].SourceOffset == 0 {
		t.Fatalf("frame metadata lost: %+v", frames[1])
	}
}

func TestMFSRejectsBadCRC(t *testing.T) {
	raw := writeMFS(sampleFrame(1, 1, "payload"))
	raw[len(raw)-1] ^= 0xFF // 翻转 CRC 末字节
	_, err := ReadAll(bytes.NewReader(raw), "bad.mfs")
	f := media.AsFault(err)
	if f == nil || f.Stage != media.StageCRC {
		t.Fatalf("want CRC fault, got %v", err)
	}
	if f.CameraID != 1 || f.Sequence != 1 || f.ByteOffset != 0 {
		t.Fatalf("fault coordinates wrong: %+v", f)
	}
}

func TestMFSRejectsTruncatedRecord(t *testing.T) {
	raw := writeMFS(sampleFrame(1, 1, "payload"))
	_, err := ReadAll(bytes.NewReader(raw[:len(raw)-10]), "trunc.mfs")
	f := media.AsFault(err)
	if f == nil || (f.Stage != media.StageLength && f.Stage != media.StagePrefix) {
		t.Fatalf("want length/prefix fault, got %v", err)
	}
}

func TestMFSRejectsTruncatedPrefix(t *testing.T) {
	_, err := ReadAll(bytes.NewReader([]byte{0x01, 0x02}), "p.mfs")
	if f := media.AsFault(err); f == nil || f.Stage != media.StagePrefix {
		t.Fatalf("want prefix fault, got %v", err)
	}
}

func TestMFSRejectsOutOfRangeLength(t *testing.T) {
	var buf bytes.Buffer
	var p [4]byte
	binary.LittleEndian.PutUint32(p[:], 99<<20) // 超过最大载荷
	buf.Write(p[:])
	_, err := ReadAll(bytes.NewReader(buf.Bytes()), "big.mfs")
	if f := media.AsFault(err); f == nil || f.Stage != media.StageLength {
		t.Fatalf("want length fault, got %v", err)
	}
}

func TestMFSRejectsUnknownVersion(t *testing.T) {
	f := sampleFrame(1, 1, "x")
	rec := media.EncodeRecord(f)
	// 仅替换版本字段：解析器必须在版本阶段拒绝，而非落到校验和阶段。
	rec[0] = 9
	if _, fault := validateRecord(rec, 0, "v"); fault == nil || fault.Stage != media.StageVersion {
		t.Fatalf("want version fault, got %v", fault)
	}
}

func TestMFSEmptyStream(t *testing.T) {
	frames, err := ReadAll(bytes.NewReader(nil), "empty.mfs")
	if err != io.EOF && err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("empty stream yielded frames")
	}
}

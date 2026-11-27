package demux

import (
	"archive/tar"
	"bytes"
	"testing"

	"relay/internal/media"
)

// buildMTA 把帧打包为 MTA/1 TAR 字节流。
func buildMTA(frames ...*media.Frame) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, f := range frames {
		rec := media.EncodeRecord(f)
		hdr := &tar.Header{
			Name: memberName(i, f),
			Mode: 0o644,
			Size: int64(len(rec)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if _, err := tw.Write(rec); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func memberName(i int, f *media.Frame) string {
	return tarName(f.CameraID, f.Sequence)
}

// tarName 与 remux 侧保持一致的确定性成员命名。
func tarName(cam uint16, seq uint64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 0, 22)
	b = append(b, "cam-"...)
	for s := 12; s >= 0; s -= 4 {
		b = append(b, digits[(cam>>uint(s))&0xF])
	}
	b = append(b, "-seq-"...)
	var n [16]byte
	for i := 15; i >= 0; i-- {
		n[i] = digits[(seq>>uint((15-i)*4))&0xF]
	}
	b = append(b, n[:]...)
	return string(b) + ".rec"
}

func TestMTARoundTrip(t *testing.T) {
	raw := buildMTA(sampleFrame(3, 10, "hello"), sampleFrame(4, 11, "world"))
	frames, err := ReadAllTAR(bytes.NewReader(raw), "arc.mta")
	if err != nil {
		t.Fatalf("ReadAllTAR: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d", len(frames))
	}
	if frames[0].CameraID != 3 || string(frames[0].Payload) != "hello" {
		t.Fatalf("frame0 mismatch: %+v", frames[0])
	}
	if frames[0].SourceOffset != 0 || frames[1].SourceOffset != 1024 {
		t.Fatalf("tar offsets wrong: %d %d", frames[0].SourceOffset, frames[1].SourceOffset)
	}
}

func TestMTARejectsCRCFault(t *testing.T) {
	raw := buildMTA(sampleFrame(3, 10, "hello-payload"))
	// 首成员：512 字节 tar 头之后是线格式记录，再偏移 HeaderLen 即载荷首字节。
	idx := 512 + media.HeaderLen
	raw[idx] ^= 0x01
	_, err := ReadAllTAR(bytes.NewReader(raw), "bad.mta")
	f := media.AsFault(err)
	if f == nil || (f.Stage != media.StageSHA && f.Stage != media.StageCRC) {
		t.Fatalf("expected checksum fault, got %v", err)
	}
	if f.ByteOffset != 0 {
		t.Fatalf("first member offset = %d, want 0", f.ByteOffset)
	}
}

func TestMTAOpenContainerDispatch(t *testing.T) {
	frames, err := OpenContainer(bytes.NewReader(buildMTA(sampleFrame(1, 1, "x"))), "a/b.mta")
	if err != nil || len(frames) != 1 {
		t.Fatalf("mta dispatch: %v %d", err, len(frames))
	}
	frames, err = OpenContainer(bytes.NewReader(writeMFS(sampleFrame(1, 1, "x"))), "a/b.mfs")
	if err != nil || len(frames) != 1 {
		t.Fatalf("mfs dispatch: %v %d", err, len(frames))
	}
}

func TestMTAEmptyArchive(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.Close()
	frames, err := ReadAllTAR(bytes.NewReader(buf.Bytes()), "empty.mta")
	if err != nil || len(frames) != 0 {
		t.Fatalf("empty mta: %v %d", err, len(frames))
	}
}

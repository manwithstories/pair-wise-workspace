package ebml

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// 手工构造: Segment(未知长度) > Cluster(未知长度) > Timecode + SimpleBlock
func sampleStream() []byte {
	var b bytes.Buffer
	// Segment, 未知长度 (0xFF)
	b.Write([]byte{0x18, 0x53, 0x80, 0x67, 0xFF})
	// Cluster, 未知长度
	b.Write([]byte{0x1F, 0x43, 0xB6, 0x75, 0xFF})
	// Timecode, 长度 1, 值 7
	b.Write([]byte{0xE7, 0x81, 0x07})
	// SimpleBlock, 长度 5
	b.Write([]byte{0xA3, 0x85, 0x81, 0x00, 0x01, 0x80, 0xAA})
	return b.Bytes()
}

func TestReadUnknownSizeElements(t *testing.T) {
	r := NewReader(bytes.NewReader(sampleStream()))

	seg, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	if seg.ID != IDSegment || !seg.Unknown() || seg.Offset != 0 || seg.DataOffset != 5 {
		t.Fatalf("segment: %+v", seg)
	}

	cl, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	if cl.ID != IDCluster || !cl.Unknown() || cl.Offset != 5 {
		t.Fatalf("cluster: %+v", cl)
	}

	tc, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	if tc.ID != IDTimecode || tc.Size != 1 {
		t.Fatalf("timecode: %+v", tc)
	}
	data, err := r.ReadData(tc)
	if err != nil || !bytes.Equal(data, []byte{0x07}) {
		t.Fatalf("data=%x err=%v", data, err)
	}

	sb, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	data, err = r.ReadData(sb)
	if err != nil || len(data) != 5 {
		t.Fatalf("block data=%x err=%v", data, err)
	}

	if _, err := r.ReadElement(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestPushBack(t *testing.T) {
	r := NewReader(bytes.NewReader(sampleStream()))
	first, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	r.PushBack(first)
	again, err := r.ReadElement()
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("pushback mismatch: %+v vs %+v", again, first)
	}
}

func TestTruncatedData(t *testing.T) {
	full := sampleStream()
	cut := full[:len(full)-2] // 截断 SimpleBlock 数据区
	// 顺序解析到 SimpleBlock
	r := NewReader(bytes.NewReader(cut))
	var sb Element
	for {
		el, err := r.ReadElement()
		if err != nil {
			t.Fatal(err)
		}
		if el.ID == IDSimpleBlock {
			sb = el
			break
		}
		if !el.Unknown() {
			if err := r.Skip(el); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := r.ReadData(sb)
	var te *TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("want TruncatedError, got %v", err)
	}
	if te.ID != IDSimpleBlock || te.Offset != int64(len(cut)) || te.Want != 5 || te.Got != 3 {
		t.Fatalf("truncated: %+v", te)
	}
}

func TestTruncatedHeader(t *testing.T) {
	full := sampleStream()
	cut := full[:len(full)-6] // 只剩 SimpleBlock 的 ID 字节，长度字节缺失
	r := NewReader(bytes.NewReader(cut))
	for {
		el, err := r.ReadElement()
		if err != nil {
			var te *TruncatedError
			if !errors.As(err, &te) {
				t.Fatalf("want TruncatedError, got %v", err)
			}
			if te.Offset != int64(len(cut)) {
				t.Fatalf("offset=%d want %d", te.Offset, len(cut))
			}
			return
		}
		if el.ID == IDTimecode {
			_ = r.Skip(el)
		}
	}
}

func TestTooLarge(t *testing.T) {
	// SimpleBlock 声明 8 字节宽的全大长度（非全 1）
	var b bytes.Buffer
	b.Write([]byte{0xA3, 0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	r := NewReader(bytes.NewReader(b.Bytes()))
	_, err := r.ReadElement()
	var tl *TooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("want TooLargeError, got %v", err)
	}
}

func TestInvalidVINT(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte{0x00}))
	if _, err := r.ReadElement(); !errors.Is(err, ErrInvalidVINT) {
		t.Fatalf("want ErrInvalidVINT, got %v", err)
	}
}

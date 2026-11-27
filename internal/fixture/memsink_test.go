package fixture

import (
	"io"
	"testing"
)

func TestMemSinkSeekAndWrite(t *testing.T) {
	m := &memSink{}
	if _, err := m.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Seek(0, io.SeekStart); err != nil || n != 0 {
		t.Fatalf("seek start n=%d err=%v", n, err)
	}
	if _, err := m.Write([]byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	if string(m.Bytes()) != "HELLO" {
		t.Fatalf("overwrite failed: %q", m.Bytes())
	}
	if size, err := m.Seek(0, avSeekSize); err != nil || size != 5 {
		t.Fatalf("avseek size=%d err=%v", size, err)
	}
	if _, err := m.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	if string(m.Bytes()) != "HELLO!" {
		t.Fatalf("append after end: %q", m.Bytes())
	}
}

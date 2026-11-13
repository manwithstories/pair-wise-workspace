package chunker

import (
	"bytes"
	"io"
	"testing"
)

func TestChunkerBoundsAndDeterminism(t *testing.T) {
	data := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog.\n"), 12000)
	var chunks [][]byte
	s := New(bytes.NewReader(data))
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk) < MinSize && len(data) > MinSize {
			// The final chunk may be smaller than MinSize.
			continue
		}
		if len(chunk) > MaxSize {
			t.Fatalf("chunk length %d exceeds max %d", len(chunk), MaxSize)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	var second [][]byte
	s = New(bytes.NewReader(data))
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		second = append(second, chunk)
	}
	if len(chunks) != len(second) {
		t.Fatalf("chunk counts differ: %d vs %d", len(chunks), len(second))
	}
	for i := range chunks {
		if !bytes.Equal(chunks[i], second[i]) {
			t.Fatalf("chunk %d differs", i)
		}
	}
}

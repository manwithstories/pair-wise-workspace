package rtsp

import "testing"

func TestInterleavedParserAcrossReads(t *testing.T) {
	p := NewInterleavedParser()
	response := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 4\r\n\r\nSDP!")
	frame := []byte{'$', 0, 0, 3, 1, 2, 3}
	wire := append(append([]byte(nil), response...), frame...)
	var chunks []Chunk
	for _, part := range [][]byte{wire[:3], wire[3:22], wire[22:37], wire[37:]} {
		got, err := p.Feed(part)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, got...)
	}
	if len(chunks) != 2 || chunks[0].Kind != ChunkControl || chunks[1].Kind != ChunkRTP {
		t.Fatalf("chunks=%+v", chunks)
	}
	if string(chunks[0].Data) != string(response) || string(chunks[1].Data) != "\x01\x02\x03" {
		t.Fatalf("chunks=%+v", chunks)
	}
}

func TestInterleavedParserMixedRTCP(t *testing.T) {
	p := NewInterleavedParser()
	got, err := p.Feed([]byte{'$', 1, 0, 2, 9, 8, '$', 0, 0, 1, 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != ChunkRTCP || got[1].Kind != ChunkRTP {
		t.Fatalf("chunks=%+v", got)
	}
}

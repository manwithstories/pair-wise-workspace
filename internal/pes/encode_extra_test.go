package pes

import "testing"

func TestEncodeLengthField(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		pts  int64
	}{
		{"no-pts", []byte("audio"), -1},
		{"with-pts", []byte("frame"), 90_000},
		{"long", make([]byte, 500), 270_000},
	}
	for _, tc := range cases {
		raw := Encode(StreamIDJPEG, tc.pts, tc.data)
		declared := int(raw[4])<<8 | int(raw[5])
		// PES_packet_length 之后恰有 declared 字节，整包 = 6 + declared。
		if 6+declared != len(raw) {
			t.Fatalf("%s: length field=%d but packet=%d", tc.name, declared, len(raw))
		}
		pkt, _, err := parseHeader(raw)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if len(pkt.Data) != len(tc.data) {
			t.Fatalf("%s: data len=%d want=%d", tc.name, len(pkt.Data), len(tc.data))
		}
	}
}

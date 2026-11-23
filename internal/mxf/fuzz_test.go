package mxf

import "testing"

// FuzzParseKLV ensures arbitrary bytes never panic the KLV walker and that any
// accepted stream has consistent lengths.
func FuzzParseKLV(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 8))
	f.Add([]byte{0x06, 0x0e, 0x2b, 0x34})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(data)
		packets, _, err := r.ParseKLV()
		if err != nil {
			return
		}
		for _, p := range packets {
			if p.Length < 0 {
				t.Fatalf("negative packet length")
			}
			if int64(len(p.Value)) != p.Length {
				t.Fatalf("value length %d != declared %d", len(p.Value), p.Length)
			}
			if int64(len(p.Value)) > 64<<20-1 {
				t.Fatalf("packet exceeds the block limit")
			}
		}
	})
}

func FuzzReadLength(f *testing.F) {
	f.Add([]byte{0x00}, 0)
	f.Add([]byte{0x81, 0x80}, 0)
	f.Add([]byte{0x84, 0x7f, 0xff, 0xff, 0xff}, 0)
	f.Fuzz(func(t *testing.T, data []byte, pos int) {
		if pos < 0 || pos >= len(data) {
			return
		}
		_, _, _ = readLength(data, pos)
	})
}

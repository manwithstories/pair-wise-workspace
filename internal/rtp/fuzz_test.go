package rtp_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"testing"

	pionrtp "github.com/pion/rtp"

	"archivearchivist/internal/rtp"
)

func validRecord(seq uint16, ts uint32, size int) []byte {
	pkt := &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: make([]byte, size),
	}
	raw, _ := pkt.Marshal()
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, int64(seq)*1000)
	binary.Write(&out, binary.BigEndian, seq)
	binary.Write(&out, binary.BigEndian, uint32(len(raw)))
	out.Write(raw)
	return out.Bytes()
}

// FuzzDecodeCapture ensures a damaged capture file cannot crash the decoder.
func FuzzDecodeCapture(f *testing.F) {
	var seed bytes.Buffer
	seed.WriteString("ARCRTP01")
	binary.Write(&seed, binary.BigEndian, uint32(2))
	seed.Write(validRecord(1, 100, 8))
	seed.Write(validRecord(2, 200, 8))
	f.Add(seed.Bytes())

	jsonl := `{"arrival_us":1000,"rtp_b64":"` +
		base64.StdEncoding.EncodeToString(mustMarshal(3, 300, 4)) + `"}` + "\n"
	f.Add([]byte(jsonl))
	f.Add([]byte("ARCRTP01"))
	f.Add([]byte("ARCRTP01\xff\xff\xff\xff"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		pkts, err := rtp.Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Whatever decodes must survive the reorder window too.
		rtp.Reorder(pkts, rtp.Options{Window: 32})
	})
}

func mustMarshal(seq uint16, ts uint32, size int) []byte {
	pkt := &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: make([]byte, size),
	}
	raw, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

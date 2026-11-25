package remux

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"mediaarchive/internal/model"
)

// Damage mirrors one recovered event into the output index.
type Damage struct {
	Stage     string `json:"stage"`
	TrackID   int32  `json:"track_id"`
	Offset    int64  `json:"offset"`
	SampleSeq uint32 `json:"sample_seq"`
	Reason    string `json:"reason"`
}

// Stats accumulates write-side quality counters.
type Stats struct {
	Packets      int      `json:"packets"`
	PacketsKept  int      `json:"packets_kept"`
	Dropped      int      `json:"dropped_samples"`
	PayloadBytes int64    `json:"payload_bytes"`
	Damages      []Damage `json:"damages"`
	PacketHash   map[uint32][][32]byte
}

// buildFtyp serializes a compatible-brands box.
func buildFtyp(brands []string) []byte {
	var buf bytes.Buffer
	major := "isom"
	if len(brands) > 0 && len(brands[0]) == 4 {
		major = brands[0]
	}
	buf.WriteString(major)
	binary.Write(&buf, binary.BigEndian, uint32(0x200))
	seen := map[string]bool{}
	for _, b := range brands {
		if len(b) != 4 || seen[b] {
			continue
		}
		seen[b] = true
		buf.WriteString(b)
	}
	for _, b := range []string{"isom", "iso2", "mp41"} {
		if !seen[b] {
			buf.WriteString(b)
		}
	}
	return wrapRaw("ftyp", buf.Bytes())
}

// wrapRaw prepends an 8-byte 32-bit box header. An overflow is an explicit
// error rather than a silent switch to 64-bit sizes.
func wrapRaw(typ string, payload []byte) []byte {
	total := uint64(8 + len(payload))
	if total > 0xffffffff {
		panic(model.NewError(model.StageRemux,
			fmt.Sprintf("box %s too large for 32-bit size: %d", typ, total)))
	}
	out := make([]byte, 0, 8+len(payload))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(total))
	copy(hdr[4:8], typ)
	out = append(out, hdr[:]...)
	out = append(out, payload...)
	return out
}

// wrapBox wraps payload as a container box and rejects 32-bit overflows.
func wrapBox(typ string, payload []byte) ([]byte, error) {
	total := uint64(8 + len(payload))
	if total > 0xffffffff {
		return nil, model.NewError(model.StageRemux,
			fmt.Sprintf("box %s too large for 32-bit size: %d", typ, total))
	}
	return wrapRaw(typ, payload), nil
}

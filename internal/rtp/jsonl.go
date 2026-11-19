package rtp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// jsonlRecord is the line format accepted by ReadJSONL. The payload is the
// full RTP packet, base64 encoded, so the writer does not need to know RTP
// header layout.
type jsonlRecord struct {
	ArrivalUS int64  `json:"arrival_us"`
	Payload   string `json:"rtp_b64"`
}

func decodeJSONL(r io.Reader) ([]Packet, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 4<<20)
	var out []Packet
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(trimSpace(raw)) == 0 {
			continue
		}
		var rec jsonlRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		b, err := base64.StdEncoding.DecodeString(rec.Payload)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad base64 payload: %w", line, err)
		}
		pk, err := decodePacket(b)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		pk.Payload = append([]byte(nil), pk.Payload...)
		pk.ArrivalUS = rec.ArrivalUS
		out = append(out, pk)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Package recovery rebuilds packets lost in burst drops from redundant
// (FEC) data, and reports unrecoverable gaps with full context instead
// of silently compressing the timeline.
package recovery

import (
	"fmt"

	"mediaroute/internal/model"
	"mediaroute/internal/packet"
)

// Parity is a redundant copy protecting one source packet.
type Parity struct {
	Protects  uint64 // extended sequence of the protected packet
	Timestamp uint32 // RTP timestamp of the protected packet
	Payload   []byte // copy of the protected payload
}

// Recovered is a rebuilt packet.
type Recovered struct {
	Seq       uint64
	Timestamp uint32
	Payload   []byte
}

// Recoverer holds the redundant data for one track.
type Recoverer struct {
	track  string
	tsBase uint64 // RTP timestamp of extended seq 0 base
	tsStep uint64 // timestamp units per sequence step
	parity map[uint64]Parity
}

func New(track string, tsBase, tsStep uint64) *Recoverer {
	return &Recoverer{track: track, tsBase: tsBase, tsStep: tsStep,
		parity: map[uint64]Parity{}}
}

// AddParity registers redundant data for later recovery.
func (r *Recoverer) AddParity(p Parity) { r.parity[p.Protects] = p }

// TimestampFor maps an extended sequence number to its RTP timestamp.
func (r *Recoverer) TimestampFor(seq uint64) uint32 {
	return uint32(r.tsBase + seq*r.tsStep)
}

// Recover fills every gap it can from parity data. The first gap (or
// hole inside a gap) with no coverage yields an ErrGap MediaError
// carrying track, sequence and timestamp; the timeline is never
// silently compressed.
func (r *Recoverer) Recover(gaps []packet.Gap) ([]Recovered, error) {
	var out []Recovered
	for _, g := range gaps {
		for seq := g.From; seq <= g.To; seq++ {
			p, ok := r.parity[seq]
			if !ok {
				return out, &model.MediaError{
					Code: model.ErrGap, Module: "recovery", Track: r.track,
					Seq:       int64(seq),
					Timestamp: int64(r.TimestampFor(seq)),
					Msg: fmt.Sprintf("no redundant data for lost packet (gap [%d,%d])",
						g.From, g.To)}
			}
			out = append(out, Recovered{Seq: seq, Timestamp: p.Timestamp,
				Payload: p.Payload})
		}
	}
	return out, nil
}

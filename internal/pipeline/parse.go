package pipeline

import (
	"io"

	"mediapack/internal/clock"
	"mediapack/internal/ebml"
)

// BlockEvent is the parser output: a decoded block with its raw byte
// offset, the enclosing cluster timecode and the resolved absolute
// timecode in Matroska ticks (wrap handling happens downstream).
type BlockEvent struct {
	Track        uint64
	ClusterTC    int64
	RelativeTC   int64
	AbsTicks     int64
	HeaderOff    int64
	PayloadOff   int64
	PayloadLen   int64
	Payload      []byte
	Keyframe     bool
	ScaleNS      int64
}

// ParseConfig bounds parser memory.
type ParseConfig struct {
	MaxElementBytes int64
	MaxBlockBytes   int64
}

// ParseSummary reports structural counts and the final stream offset.
type ParseSummary struct {
	Clusters      int
	Blocks        int
	EndOffset     int64
	TimestampScale int64
}

// blockCallback receives every decoded block in stream order.
type blockCallback func(BlockEvent) error

// parseMatroska walks a Matroska/WebM stream depth-first, converting
// SimpleBlock/Block elements into BlockEvents while preserving absolute
// byte offsets. It streams bodies and never buffers the whole input.
func parseMatroska(r io.Reader, cfg ParseConfig, onBlock blockCallback) (sum ParseSummary, err error) {
	sum.TimestampScale = 1_000_000 // Matroska default 1ms
	maxElem := cfg.MaxElementBytes
	if maxElem <= 0 {
		maxElem = 1 << 30
	}
	maxBlock := cfg.MaxBlockBytes
	if maxBlock <= 0 {
		maxBlock = 8 << 20
	}
	rd := ebml.NewReader(r, maxElem)
	var clusterTC int64
	var scale int64 = 1_000_000
	// Record the reached byte position on every return path, including
	// interruption and corruption, so callers can checkpoint progress.
	defer func() { sum.EndOffset = rd.Offset() }()

	for {
		el, err := rd.Next()
		if err == io.EOF {
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
		switch el.ID {
		case ebml.IDSegment:
			rd.Push(el)
			if err := walkSegment(rd, &clusterTC, scale, maxBlock, onBlock, &sum); err != nil {
				return sum, err
			}
		case ebml.IDTimestampScale:
			body, err := rd.ReadBody(el, 8)
			if err != nil {
				return sum, err
			}
			scale = int64(decodeUint(body))
			if scale <= 0 {
				return sum, clock.AtError(clock.CatOverflow, el.HeaderOff,
					"invalid timestamp scale %d", scale)
			}
			sum.TimestampScale = scale
		default:
			if el.Unknown {
				rd.CloseCurrent()
				continue
			}
			if err := rd.DiscardBody(el); err != nil {
				return sum, err
			}
		}
	}
}

func walkSegment(rd *ebml.Reader, clusterTC *int64, scale, maxBlock int64,
	onBlock blockCallback, sum *ParseSummary) error {
	for {
		el, err := rd.Next()
		if err == io.EOF {
			if rd.Depth() > 0 {
				rd.CloseCurrent()
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case ebml.IDCluster:
			sum.Clusters++
			rd.Push(el)
			if err := walkCluster(rd, clusterTC, scale, maxBlock, onBlock, sum); err != nil {
				return err
			}
		case ebml.IDTimestampScale:
			body, err := rd.ReadBody(el, 8)
			if err != nil {
				return err
			}
			scale = int64(decodeUint(body))
			sum.TimestampScale = scale
		default:
			if el.Unknown {
				rd.CloseCurrent()
				continue
			}
			if err := rd.DiscardBody(el); err != nil {
				return err
			}
		}
	}
}

func walkCluster(rd *ebml.Reader, clusterTC *int64, scale, maxBlock int64,
	onBlock blockCallback, sum *ParseSummary) error {
	for {
		el, err := rd.Next()
		if err == io.EOF {
			if rd.Depth() > 0 {
				rd.CloseCurrent()
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch el.ID {
		case ebml.IDTimecode:
			body, err := rd.ReadBody(el, 8)
			if err != nil {
				return err
			}
			*clusterTC = int64(decodeUint(body))
		case ebml.IDSimpleBlock, ebml.IDBlock:
			body, err := rd.ReadBody(el, maxBlock)
			if err != nil {
				return err
			}
			blk, err := ebml.DecodeBlock(body, el.HeaderOff, el.ID == ebml.IDSimpleBlock)
			if err != nil {
				return err
			}
			blk.SetPayloadOff(el.BodyOff, 0)
			abs := *clusterTC + int64(blk.RelativeTC)
			ev := BlockEvent{
				Track:      blk.Track,
				ClusterTC:  *clusterTC,
				RelativeTC: int64(blk.RelativeTC),
				AbsTicks:   abs,
				HeaderOff:  blk.HeaderOff,
				PayloadOff: blk.PayloadOff,
				PayloadLen: blk.PayloadLen,
				Payload:    append([]byte(nil), blk.Payload...),
				Keyframe:   blk.Keyframe,
				ScaleNS:    scale,
			}
			sum.Blocks++
			if err := onBlock(ev); err != nil {
				return err
			}
		default:
			if el.Unknown {
				rd.CloseCurrent()
				continue
			}
			if err := rd.DiscardBody(el); err != nil {
				return err
			}
		}
	}
}

// decodeUint reads a big-endian unsigned integer of 1..8 bytes.
func decodeUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = (v << 8) | uint64(c)
	}
	return v
}

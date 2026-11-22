// Package fixture generates all sample data in code: a valid
// three-track EBML stream sized to a target bitrate, a corrupted
// (truncated) EBML stream, and RTP packet streams exercising
// duplicates, reordering, sequence wraparound and loss.
package fixture

// TrackInfo describes one generated track.
type TrackInfo struct {
	ID   uint64
	Kind string
}

// Tracks is the fixed track layout of generated samples.
func Tracks() []TrackInfo {
	return []TrackInfo{
		{ID: 1, Kind: "video"},
		{ID: 2, Kind: "audio"},
		{ID: 3, Kind: "subtitle"},
	}
}

// EBML element IDs (mirrors internal/ebml to keep fixture standalone).
const (
	idSegment       = 0x1A45DFA3
	idInfo          = 0x1549A966
	idTimeScale     = 0x2AD7B1
	idTracks        = 0x1654AE6B
	idCluster       = 0x1F43B675
	idTimestamp     = 0xE7
	idBlockGroup    = 0xA0
	idBlock         = 0xA1
	idBlockDuration = 0x9B
	idVoid          = 0xEC
)

const timeScaleNs = 1_000_000 // 1 tick = 1 ms

// splitmix64 deterministic byte stream.
type rng uint64

func (s *rng) next() uint64 {
	*s += 0x9E3779B97F4A7C15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xBF58476C1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (s *rng) bytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		if i%8 == 0 {
			s.next()
		}
		out[i] = byte(uint64(*s) >> (8 * (i % 8)))
	}
	return out
}

func vintID(id uint64) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// vintSize encodes a size VINT; width 0 picks the minimal width,
// reserving the all-ones pattern for "unknown".
func vintSize(size uint64, width int) []byte {
	if width == 0 {
		for w := 1; w <= 8; w++ {
			if size < (uint64(1)<<(7*w))-1 {
				width = w
				break
			}
		}
	}
	out := make([]byte, width)
	s := size
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(s)
		s >>= 8
	}
	out[0] |= 1 << (8 - width)
	return out
}

func unknownSize(width int) []byte {
	out := make([]byte, width)
	for i := range out {
		out[i] = 0xFF
	}
	out[0] = 0xFF >> (width - 1)
	return out
}

func element(id uint64, payload []byte) []byte {
	out := vintID(id)
	out = append(out, vintSize(uint64(len(payload)), 0)...)
	return append(out, payload...)
}

func uintElement(id uint64, v uint64) []byte {
	var b [8]byte
	n := 0
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
		if v == 0 {
			n = i
			break
		}
	}
	return element(id, b[n:])
}

func blockGroup(track uint64, ts int16, dur uint64, payload []byte) []byte {
	b := vintSize(track, 0)
	b = append(b, byte(uint16(ts)>>8), byte(uint16(ts)))
	b = append(b, 0x80) // keyframe, no lacing
	b = append(b, payload...)
	inner := element(idBlock, b)
	inner = append(inner, uintElement(idBlockDuration, dur)...)
	return element(idBlockGroup, inner)
}

func cluster(tsMs uint64, sized bool, blocks ...[]byte) []byte {
	payload := uintElement(idTimestamp, tsMs)
	for _, b := range blocks {
		payload = append(payload, b...)
	}
	if sized {
		return element(idCluster, payload)
	}
	out := vintID(idCluster)
	out = append(out, unknownSize(2)...)
	return append(out, payload...)
}

// distribute splits total into n parts differing by at most one byte.
func distribute(total int64, n int) []int64 {
	out := make([]int64, n)
	base := total / int64(n)
	rem := total % int64(n)
	for i := range out {
		out[i] = base
		if int64(i) < rem {
			out[i]++
		}
	}
	return out
}

// BuildEBML generates a deterministic three-track EBML stream whose
// total size in bytes is exactly targetBps*durationNs/8e9, so the
// delivered bitrate lands on the target. The Segment and every fourth
// Cluster use unknown (streaming) sizes.
func BuildEBML(seed int64, durationNs, targetBps int64) ([]byte, []TrackInfo) {
	totalBytes := targetBps * durationNs / 8_000_000_000
	durMs := durationNs / 1_000_000
	nClusters := int(durMs / 500)
	if nClusters < 2 {
		nClusters = 2
	}

	// Reserve headroom for container overhead and the padding element.
	media := totalBytes - 64*1024
	video := distribute(media*80/100, nClusters)
	audio := distribute(media*15/100, nClusters*2)
	sub := distribute(media*5/100, nClusters/2)

	r := rng(seed)
	var body []byte
	body = append(body, element(idInfo, uintElement(idTimeScale, timeScaleNs))...)
	body = append(body, element(idTracks, nil)...)
	vi, ai, si := 0, 0, 0
	for c := 0; c < nClusters; c++ {
		var blocks [][]byte
		blocks = append(blocks, blockGroup(1, 0, 500, r.bytes(int(video[vi]))))
		vi++
		blocks = append(blocks, blockGroup(2, 0, 250, r.bytes(int(audio[ai]))))
		ai++
		blocks = append(blocks, blockGroup(2, 250, 250, r.bytes(int(audio[ai]))))
		ai++
		if c%2 == 0 {
			blocks = append(blocks, blockGroup(3, 0, 1000, r.bytes(int(sub[si]))))
			si++
		}
		sized := c%4 != 1 || c == nClusters-1
		body = append(body, cluster(uint64(c*500), sized, blocks...)...)
	}

	// Pad with a Void element so the file is exactly totalBytes long.
	header := len(vintID(idSegment)) + 8
	pad := int(totalBytes) - header - len(body)
	if pad < 2 {
		panic("fixture: container overhead exceeds budget")
	}
	var void []byte
	for w := 1; w <= 8; w++ {
		payload := pad - 1 - w
		if payload >= 0 && uint64(payload) < (uint64(1)<<(7*w))-1 {
			void = vintID(idVoid)
			void = append(void, vintSize(uint64(payload), w)...)
			void = append(void, make([]byte, payload)...)
			break
		}
	}
	body = append(body, void...)

	out := vintID(idSegment)
	out = append(out, unknownSize(8)...)
	return append(out, body...), Tracks()
}

// Corrupted returns an EBML stream whose final element has a truncated
// (two-byte, one present) size VINT.
func Corrupted() []byte {
	out := vintID(idSegment)
	out = append(out, unknownSize(8)...)
	out = append(out, element(idInfo, uintElement(idTimeScale, timeScaleNs))...)
	out = append(out, vintID(idTimestamp)...)
	out = append(out, 0x40) // size VINT declares width 2, second byte missing
	return out
}

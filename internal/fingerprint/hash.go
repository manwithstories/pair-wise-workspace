// Package fingerprint computes reproducible content fingerprints over
// fragmented media using deterministic RTP packetization and finds duplicate
// content across tracks while keeping every source mapping.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"sort"

	"github.com/pion/rtp"

	"mediad/internal/model"
)

// BlockSize is the fixed payload chunk fed to one RTP packet.
const BlockSize = 1100

// Source locates one piece of fingerprinted content.
type Source struct {
	Track model.TrackRole `json:"track"`
	Index int             `json:"index"`
	URI   string          `json:"uri"`
}

func (s Source) String() string { return fmt.Sprintf("%s#%d(%s)", s.Track, s.Index, s.URI) }

// Fingerprint is the deterministic identity of one fragment.
type Fingerprint struct {
	Source Source `json:"source"`
	// FullDigest covers every byte of the fragment plus its declared length.
	FullDigest [32]byte `json:"-"`
	DigestHex  string   `json:"digest_hex"`
	// Blocks are ordered FNV-1a hashes of fixed-size payload blocks.
	Blocks   []uint64 `json:"-"`
	BlockHex []string `json:"block_hex,omitempty"`
	Bytes    int64    `json:"bytes"`
	// Truncated marks fragments whose declared structure exceeds real bytes;
	// such content may never merge with complete fragments.
	Truncated bool `json:"truncated"`
}

// fixedPayloader splits payloads into deterministic, equal-size chunks.
type fixedPayloader struct{ size int }

func (p fixedPayloader) Payload(mtu uint16, payload []byte) [][]byte {
	size := p.size
	if size <= 0 || size > int(mtu) {
		size = int(mtu)
	}
	var out [][]byte
	for len(payload) > 0 {
		n := min(size, len(payload))
		chunk := make([]byte, n)
		copy(chunk, payload[:n])
		out = append(out, chunk)
		payload = payload[n:]
	}
	return out
}

// FingerprintReader packetizes r into deterministic RTP packets and folds
// their marshaled bytes into the fragment digest. truncated marks an input
// known to be cut short; the fingerprint then only joins truncated groups.
func FingerprintReader(r io.Reader, src Source, truncated bool) (*Fingerprint, error) {
	pk := rtp.NewPacketizer(BlockSize+12, 96, 1, fixedPayloader{size: BlockSize},
		rtp.NewFixedSequencer(0), 48000)
	full := sha256.New()
	var blocks []uint64
	var total, pktNo int64
	buf := make([]byte, BlockSize)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := buf[:n]
			total += int64(n)
			for _, pkt := range pk.Packetize(chunk, uint32(n)) {
				// NewPacketizer seeds the RTP timestamp randomly; rewrite it
				// to a deterministic value so reruns produce identical bytes.
				pkt.Timestamp = uint32(pktNo * BlockSize)
				pktNo++
				raw, merr := pkt.Marshal()
				if merr != nil {
					return nil, merr
				}
				full.Write(raw)
			}
			h := fnv.New64a()
			h.Write(chunk)
			blocks = append(blocks, h.Sum64())
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(total))
	full.Write(lenBuf[:]) // defeat prefix-collision between different lengths
	fp := &Fingerprint{
		Source: src, FullDigest: [32]byte(full.Sum(nil)),
		Blocks: blocks, Bytes: total, Truncated: truncated,
	}
	fp.DigestHex = fmt.Sprintf("%x", fp.FullDigest[:])
	fp.BlockHex = make([]string, len(blocks))
	for i, b := range blocks {
		fp.BlockHex[i] = fmt.Sprintf("%016x", b)
	}
	return fp, nil
}

// DuplicateGroup gathers fragments judged to carry identical content.
type DuplicateGroup struct {
	DigestHex string   `json:"digest_hex"`
	Sources   []Source `json:"sources"`
	Bytes     int64    `json:"bytes"`
	Truncated bool     `json:"truncated"`
}

// FindDuplicates groups fingerprints whose full digest matches. Truncated
// fragments only ever group with other truncated fragments, so a cut prefix
// of a complete fragment can never be merged into it.
func FindDuplicates(fps []*Fingerprint) []DuplicateGroup {
	groups := map[string]*DuplicateGroup{}
	order := make([]string, 0)
	for _, fp := range fps {
		key := fp.DigestHex
		if fp.Truncated {
			key = "t:" + key
		}
		g, ok := groups[key]
		if !ok {
			g = &DuplicateGroup{DigestHex: fp.DigestHex, Bytes: fp.Bytes, Truncated: fp.Truncated}
			groups[key] = g
			order = append(order, key)
		}
		g.Sources = append(g.Sources, fp.Source)
	}
	var out []DuplicateGroup
	for _, k := range order {
		if len(groups[k].Sources) > 1 {
			sort.Slice(groups[k].Sources, func(i, j int) bool {
				if groups[k].Sources[i].Track != groups[k].Sources[j].Track {
					return groups[k].Sources[i].Track < groups[k].Sources[j].Track
				}
				return groups[k].Sources[i].Index < groups[k].Sources[j].Index
			})
			out = append(out, *groups[k])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DigestHex < out[j].DigestHex })
	return out
}

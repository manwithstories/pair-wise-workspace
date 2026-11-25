package fixture

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"

	"mediaarchive/internal/model"
)

// armf payload: magic + chunk seq + sample count (mirrors demux expectations).
func buildArmf(seq uint32, count int) []byte {
	var p bytes.Buffer
	p.WriteString("ARMF")
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], seq)
	p.Write(b[:])
	binary.BigEndian.PutUint32(b[:], uint32(count))
	p.Write(b[:])
	return box("armf", p.Bytes())
}

// buildAndWrite synthesizes one container end to end.
func buildAndWrite(spec Spec, path string) error {
	b := &builder{spec: spec, rng: rand.New(rand.NewSource(spec.Seed))}
	if err := b.synthesize(); err != nil {
		return err
	}
	data, err := b.serialize()
	if err != nil {
		return err
	}
	if spec.TruncateTail > 0 {
		if int64(len(data)) <= spec.TruncateTail {
			return model.NewError(model.StageCLI, "truncate tail larger than file")
		}
		data = data[:int64(len(data))-spec.TruncateTail]
	}
	return os.WriteFile(path, data, 0o644)
}

// serialize lays out ftyp + moov(armd) + [armf + mdat]* and resolves every
// absolute sample offset. The armd length is fixed (offset fields are always
// 8 bytes), so a two-pass build yields exact mdat positions.
func (b *builder) serialize() ([]byte, error) {
	ftyp := b.buildFtyp()
	armd0 := b.buildArmd()
	moov0 := box("moov", box("armd", armd0))
	// Pass 1: measure each chunk's armf+mdat position.
	pos := len(ftyp) + len(moov0)
	chunkPos := make([]int, len(b.tables))
	for ci, table := range b.tables {
		chunkPos[ci] = pos
		pos += len(buildArmf(uint32(ci), len(table)))
		pos += 8 // mdat header
		for _, rec := range table {
			pos += len(rec.payload)
		}
	}
	// Pass 2: rebuild armd with exact per-chunk offsets.
	armd := b.buildArmd()
	b.resolveOffsets(armd, chunkPos)
	moov := box("moov", box("armd", armd))
	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(moov)
	for ci, table := range b.tables {
		out.Write(buildArmf(uint32(ci), len(table)))
		var body bytes.Buffer
		for _, rec := range table {
			body.Write(rec.payload)
		}
		out.Write(box("mdat", body.Bytes()))
	}
	return out.Bytes(), nil
}

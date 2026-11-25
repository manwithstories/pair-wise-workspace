package fixture

import (
	"bytes"
	"math/rand"
	"os"

	"mediaarchive/internal/model"
)

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

// serialize lays out ftyp + moov(armd) + mdat and resolves absolute offsets.
func (b *builder) serialize() ([]byte, error) {
	ftyp := b.buildFtyp()
	// First build fixes armd serialized length (offset fields are always
	// 8 bytes), which determines the exact mdat payload position.
	armd0 := b.buildArmd()
	moov0 := box("moov", box("armd", armd0))
	mdatPos := len(ftyp) + len(moov0) + 8
	b.resolveOffsets(b.buildArmd(), mdatPos)
	var mdat bytes.Buffer
	for _, rec := range b.tables[0] {
		mdat.Write(rec.payload)
	}
	out := make([]byte, 0, len(ftyp)+len(moov0)+8+mdat.Len())
	out = append(out, ftyp...)
	out = append(out, box("moov", box("armd", b.lastArmd))...)
	out = append(out, box("mdat", mdat.Bytes())...)
	return out, nil
}

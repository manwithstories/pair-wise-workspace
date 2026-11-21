package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"mediad/internal/probe"
)

// MXFTrack describes one track in a generated MXF file.
type MXFTrack struct {
	TrackID   uint32
	Video     bool
	EditRateN int32
	EditRateD int32
	// EditUnits is the track duration in edit units.
	EditUnits uint64
	// StartPosition is the origin in edit units (usually 0).
	StartPosition uint64

	StoredWidth, StoredHeight uint32
	Rotation                  int32
	HorizCrop, VertCrop       uint32
	Color                     *ColorSpec
	// AudioRate is the sampling rate for sound tracks.
	AudioRate int32

	// EssenceBytes is the payload size of each edit unit.
	EssenceBytes int
}

// MXFSpec describes an MXF fixture file.
type MXFSpec struct {
	Tracks []MXFTrack
	Name   string
}

// WriteMXF writes a self-contained MXF profile file and returns its size.
func WriteMXF(path string, spec MXFSpec, seed int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	data, err := buildMXF(spec, seed)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// klv serialises one key/length/value triple.
func klv(key probe.UL, value []byte) []byte {
	out := make([]byte, 0, 16+9+len(value))
	out = append(out, key[:]...)
	out = append(out, probe.EncodeBER(uint64(len(value)))...)
	return append(out, value...)
}

// localSet serialises a set from ordered (tag, value) pairs.
type item struct {
	tag uint16
	val []byte
}

func localSet(key probe.UL, items []item) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, byte(it.tag>>8), byte(it.tag))
		body = append(body, probe.EncodeBER(uint64(len(it.val)))...)
		body = append(body, it.val...)
	}
	return klv(key, body)
}

func mustTag(name string) uint16 {
	t, ok := probe.TagFor(name)
	if !ok {
		panic("unknown MXF property " + name)
	}
	return t
}

// buildMXF lays out header metadata, essence and an index table.
func buildMXF(spec MXFSpec, seed int64) ([]byte, error) {
	if len(spec.Tracks) == 0 {
		return nil, fmt.Errorf("MXF fixture needs at least one track")
	}
	rng := rand.New(rand.NewSource(seed ^ 0x4d5846))

	umid := func(n byte) [32]byte {
		var u [32]byte
		copy(u[:4], []byte{0x06, 0x0a, 0x2b, 0x34})
		u[4] = n
		for i := 5; i < 32; i++ {
			u[i] = byte(rng.Intn(256))
		}
		return u
	}
	contentUMID := umid(0x10)
	pkgUMID := umid(0x20)
	seqUMIDs := make([][32]byte, len(spec.Tracks))
	clipUMIDs := make([][32]byte, len(spec.Tracks))
	tcUMIDs := make([][32]byte, len(spec.Tracks))
	descUMIDs := make([][32]byte, len(spec.Tracks))
	trackUMIDs := make([][32]byte, len(spec.Tracks))
	for i := range spec.Tracks {
		seqUMIDs[i] = umid(byte(0x30 + i))
		clipUMIDs[i] = umid(byte(0x40 + i))
		tcUMIDs[i] = umid(byte(0x50 + i))
		descUMIDs[i] = umid(byte(0x60 + i))
		trackUMIDs[i] = umid(byte(0x70 + i))
	}

	ulContent := probe.MustUL("060e2b34.02530101.0d010101.01011800")
	ulPackage := probe.MustUL("060e2b34.02530101.0d010101.01033600")
	ulTrack := probe.MustUL("060e2b34.02530101.0d010101.01031200")
	ulSequence := probe.MustUL("060e2b34.02530101.0d010101.01010f00")
	ulClip := probe.MustUL("060e2b34.02530101.0d010101.01011100")
	ulTC := probe.MustUL("060e2b34.02530101.0d010101.01011400")
	ulCDCI := probe.MustUL("060e2b34.02530101.0d010101.01012800")
	ulWave := probe.MustUL("060e2b34.02530101.0d010101.01012900")

	// References from the package to its tracks.
	var trackRefs []byte
	trackRefs = binary.BigEndian.AppendUint32(trackRefs, uint32(len(spec.Tracks)))
	for i, t := range spec.Tracks {
		trackRefs = append(trackRefs, probe.EncodeStrongRef(ulTrack, trackUMIDs[i], t.TrackID)...)
	}

	var sets [][]byte
	// Preface.
	opPattern := probe.MustUL("060e2b34.04010101.0d010201.01030100")
	pref := []item{
		{mustTag("OperationalPattern"), append([]byte(nil), opPattern[:]...)},
		{mustTag("ContentStorageRef"), probe.EncodeStrongRef(ulContent, contentUMID, 0)},
	}
	sets = append(sets, localSet(probe.ULPreface, pref))

	// ContentStorage holds the package list.
	sets = append(sets, localSet(ulContent, []item{
		{mustTag("PackageUMID"), contentUMID[:]},
		{mustTag("Packages"), func() []byte {
			b := binary.BigEndian.AppendUint32(nil, 1)
			return append(b, probe.EncodeStrongRef(ulPackage, pkgUMID, 0)...)
		}()},
	}))

	// Source package.
	sets = append(sets, localSet(ulPackage, []item{
		{mustTag("PackageUMID"), pkgUMID[:]},
		{mustTag("PackageName"), probe.AppendUTF16(nil, spec.Name)},
		{mustTag("Tracks"), trackRefs},
	}))

	// Per-track sets.
	for i, t := range spec.Tracks {
		dd := probe.ULDataDefPicture
		if !t.Video {
			dd = probe.ULDataDefSound
		}
		editRate := probe.EncodeRational(t.EditRateN, t.EditRateD)
		sets = append(sets, localSet(ulTrack, []item{
			{mustTag("PackageUMID"), trackUMIDs[i][:]},
			{mustTag("TrackID"), probe.EncodeUint32(t.TrackID)},
			{mustTag("EditRate"), editRate},
			{mustTag("Duration"), probe.EncodeUint64(t.EditUnits)},
			{mustTag("StartPosition"), probe.EncodeUint64(t.StartPosition)},
			{mustTag("DataDefinition"), dd[:]},
			{mustTag("SequenceRef"), probe.EncodeStrongRef(ulSequence, seqUMIDs[i], t.TrackID)},
			{mustTag("DescriptorRef"), probe.EncodeStrongRef(descUL(t.Video), descUMIDs[i], 0)},
			{mustTag("TimecodeRef"), probe.EncodeStrongRef(ulTC, tcUMIDs[i], 0)},
		}))
		sets = append(sets, localSet(ulSequence, []item{
			{mustTag("PackageUMID"), seqUMIDs[i][:]},
			{mustTag("DataDefinition"), dd[:]},
			{mustTag("Components"), func() []byte {
				b := binary.BigEndian.AppendUint32(nil, 1)
				return append(b, probe.EncodeStrongRef(ulClip, clipUMIDs[i], t.TrackID)...)
			}()},
		}))
		sets = append(sets, localSet(ulClip, []item{
			{mustTag("PackageUMID"), clipUMIDs[i][:]},
			{mustTag("DataDefinition"), dd[:]},
			{mustTag("SourceTrackID"), probe.EncodeUint32(t.TrackID)},
			{mustTag("StartPosition"), probe.EncodeUint64(t.StartPosition)},
			{mustTag("Duration"), probe.EncodeUint64(t.EditUnits)},
		}))
		sets = append(sets, localSet(ulTC, []item{
			{mustTag("PackageUMID"), tcUMIDs[i][:]},
			{mustTag("StartTimecode"), probe.EncodeUint64(t.StartPosition)},
			{mustTag("RoundedTimecodeBase"), probe.EncodeUint32(uint32((t.EditRateN + t.EditRateD/2) / t.EditRateD))},
			{mustTag("DropFrame"), probe.EncodeUint32(0)},
		}))

		if t.Video {
			items := []item{
				{mustTag("PackageUMID"), descUMIDs[i][:]},
				{mustTag("StoredWidth"), probe.EncodeUint32(t.StoredWidth)},
				{mustTag("StoredHeight"), probe.EncodeUint32(t.StoredHeight)},
				{mustTag("RotationDegrees"), probe.EncodeUint32(uint32(t.Rotation))},
			}
			if t.HorizCrop > 0 {
				items = append(items, item{mustTag("HorizCrop"), probe.EncodeUint32(t.HorizCrop)})
			}
			if t.VertCrop > 0 {
				items = append(items, item{mustTag("VertCrop"), probe.EncodeUint32(t.VertCrop)})
			}
			if t.Color != nil {
				items = append(items,
					item{mustTag("ColorPrimaries"), probe.EncodeUint32(uint32(t.Color.Primaries))},
					item{mustTag("TransferCharacteristic"), probe.EncodeUint32(uint32(t.Color.Transfer))},
					item{mustTag("MatrixCoefficient"), probe.EncodeUint32(uint32(t.Color.Matrix))},
					item{mustTag("FullRangeFlag"), probe.EncodeUint32(boolU32(t.Color.FullRange))},
				)
			}
			sets = append(sets, localSet(ulCDCI, items))
		} else {
			sets = append(sets, localSet(ulWave, []item{
				{mustTag("PackageUMID"), descUMIDs[i][:]},
				{mustTag("AudioSamplingRate"), probe.EncodeRational(t.AudioRate, 1)},
				{mustTag("ChannelCount"), probe.EncodeUint32(2)},
				{mustTag("QuantizationBits"), probe.EncodeUint32(24)},
			}))
		}
	}

	// Primer pack.
	primerPayload := binary.BigEndian.AppendUint32(nil, uint32(len(probe.PrimerCatalogue())))
	for _, e := range probe.PrimerCatalogue() {
		primerPayload = binary.BigEndian.AppendUint16(primerPayload, e.Tag)
		primerPayload = append(primerPayload, e.UL[:]...)
	}
	primerKLV := klv(probe.ULPrimerPack, primerPayload)

	// Header partition pack: run-in (4 bytes) + u32 length + local set.
	var headerSet []byte
	headerSet = binary.BigEndian.AppendUint32(headerSet, 1)   // major version
	headerSet = binary.BigEndian.AppendUint32(headerSet, 0)   // local tag count
	partitionBody := append([]byte{0, 0, 0, 0}, headerSet...) // run-in = 0
	headerPartition := klv(probe.ULHeaderPartition, partitionBody)

	// Essence: one element per track, seeded bytes. Elements are contiguous
	// so a reader can attribute every byte to its track.
	var essences [][]byte
	unit := make([]byte, 0, 8192)
	for _, t := range spec.Tracks {
		size := int(t.EditUnits) * t.EssenceBytes
		if t.EssenceBytes <= 0 || size <= 0 || size > 64<<20 {
			return nil, fmt.Errorf("track %d essence size %d out of range", t.TrackID, size)
		}
		buf := make([]byte, 0, size)
		for i := 0; i < int(t.EditUnits); i++ {
			unit = Payload(seed, t.TrackID, i, t.EssenceBytes, unit)
			buf = append(buf, unit...)
		}
		essences = append(essences, klv(probe.EssenceKey(t.Video, t.TrackID), buf))
	}

	// Index table describing the first track.
	indexKLV := klv(probe.ULIndexTable, []byte{byte(len(spec.Tracks))})

	body := make([]byte, 0, 4096)
	body = append(body, headerPartition...)
	body = append(body, primerKLV...)
	for _, s := range sets {
		body = append(body, s...)
	}
	body = append(body, klv(probe.ULBodyPartition, append([]byte{0, 0, 0, 0}, headerSet...))...)
	for _, e := range essences {
		body = append(body, e...)
	}
	body = append(body, indexKLV...)
	body = append(body, klv(probe.ULFooterPartition, append([]byte{0, 0, 0, 0}, headerSet...))...)
	return body, nil
}

func descUL(video bool) probe.UL {
	if video {
		return probe.MustUL("060e2b34.02530101.0d010101.01012800")
	}
	return probe.MustUL("060e2b34.02530101.0d010101.01012900")
}

func boolU32(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

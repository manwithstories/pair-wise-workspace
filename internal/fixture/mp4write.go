// Package fixture generates the media, manifests and RTP streams used by the
// demo and tests. Everything is produced in-process; no external files or
// network resources are consulted.
package fixture

// MP4Track describes one track in a generated fragmented MP4.
type MP4Track struct {
	ID         uint32
	Handler    string // "vide" or "soun"
	Timescale  uint32
	Width      uint16
	Height     uint16
	Channels   uint16
	SampleRate uint32
	Primaries  uint16 // nclx colour primaries, video only (0 omits colr)
	Transfer   uint16
	Matrix     uint16
	FullRange  bool
	ChanConfig uint8 // AAC channelConfiguration, audio only
	ObjectType uint8 // esds objectTypeIndication, audio only
}

// MP4Sample is one sample in a fragment.
type MP4Sample struct {
	Duration uint32
	Data     []byte
	Sync     bool
}

// MP4Fragment is a moof+mdat pair for one track.
type MP4Fragment struct {
	TrackID        uint32
	BaseDecodeTime uint64
	Samples        []MP4Sample
}

// BuildFragmentedMP4 assembles a minimal but structurally valid fragmented
// ISO-BMFF file. The moov declares every track and its sample entry; each
// fragment carries its own mdat and a tfdt with the 64-bit decode time.
func BuildFragmentedMP4(tracks []MP4Track, frags []MP4Fragment) []byte {
	var out []byte
	out = append(out, box("ftyp", concat([][]byte{[]byte("iso5"), u32(0), []byte("iso5"), []byte("iso6"), []byte("mp41")}))...)

	var traks [][]byte
	for _, t := range tracks {
		traks = append(traks, buildTrak(t))
	}
	out = append(out, box("moov", concat([][]byte{buildMvhd(tracks), concat(traks), buildMvex(tracks)}))...)

	for i, f := range frags {
		// Under default-base-is-moof the trun data offset is measured from the
		// moof start; the mdat header follows the moof, so the payload begins
		// at len(moof)+8.
		probe, _ := buildMoofWithOffset(f, uint32(i+1), 0)
		moof, _ := buildMoofWithOffset(f, uint32(i+1), int32(len(probe))+8)
		out = append(out, moof...)
		out = append(out, box("mdat", concatSamples(f.Samples))...)
	}
	return out
}

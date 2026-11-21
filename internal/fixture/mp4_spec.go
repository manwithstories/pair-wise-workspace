package fixture

// TrackSpec describes one synthetic track.
type TrackSpec struct {
	ID         uint32
	Kind       string // "video" or "audio"
	Codec      string // "avc1" or "mp4a"
	Timescale  uint32
	Frames     int
	FrameDelta uint32
	Width      uint16
	Height     uint16

	// Rotation is written into tkhd as a right-angle rotation.
	Rotation int
	// CropLeft/CropRight/CropTop/CropBottom are written as a clap box.
	CropLeft, CropRight, CropTop, CropBottom uint32
	// Color is written as a colr nclx box; nil leaves it out.
	Color *ColorSpec
	// ConflictColor, when set, is written as a second, disagreeing colr
	// box so the arbitration path can be exercised.
	ConflictColor *ColorSpec
	// SampleBytes is the payload size of each sample.
	SampleBytes int
	// GOPLength > 0 makes only every GOPLength-th sample a sync sample.
	GOPLength int
}

// ColorSpec is a container-level colour description.
type ColorSpec struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// MP4Spec describes an MP4 fixture file.
type MP4Spec struct {
	Tracks []TrackSpec
	// EditMediaTime, when non-zero, is written as an elst media_time, so the
	// track's timeline starts that many ticks into the media.
	EditMediaTime int32
}

// WriteMP4 writes spec to path, returning the file size.

package segment

// Profile describes one rendition (bitrate ladder rung).
//
// Bitrate is expressed in bits per second and is always a multiple of 8000,
// so size(bytes)*1000/bitrate stays an exact integer number of milliseconds:
// this keeps "measured duration" arithmetic deterministic in tests and demos.
type Profile struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	BitrateBps int    `json:"bitrate_bps"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

// Profiles is the fixed four-rung ladder used by builder and fixture.
var Profiles = []Profile{
	{Index: 0, Name: "240p", BitrateBps: 400000, Width: 426, Height: 240},
	{Index: 1, Name: "360p", BitrateBps: 800000, Width: 640, Height: 360},
	{Index: 2, Name: "540p", BitrateBps: 1600000, Width: 960, Height: 540},
	{Index: 3, Name: "720p", BitrateBps: 3200000, Width: 1280, Height: 720},
}

// ProfileByIndex returns the ladder rung for a rendition index.
// The second return value is false for unknown indices.
func ProfileByIndex(index int) (Profile, bool) {
	for _, profile := range Profiles {
		if profile.Index == index {
			return profile, true
		}
	}
	return Profile{}, false
}

// MeasuredDurationMS derives the actual playback duration of a segment from
// its on-disk size and the rendition bitrate. A real pipeline would read the
// container timestamps; without ffmpeg the size/bitrate estimate is the local,
// dependency-free equivalent and is used for drift detection.
func MeasuredDurationMS(sizeBytes int64, bitrateBps int) int64 {
	if bitrateBps <= 0 {
		return 0
	}
	return sizeBytes * 8000 / int64(bitrateBps)
}

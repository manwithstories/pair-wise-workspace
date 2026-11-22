// Audio codec adapters: linear PCM (sowt/twos/lpcm) and AAC metadata.
//
// PCM samples are decoded to interleaved float64 frames in [-1, 1]. AAC
// AudioSpecificConfig is parsed into a normalised Format so that the engine
// can report exact parameters; compressed payloads are not decoded in this
// build and fail with a precise UNSUPPORTED_CODEC rather than bad audio.
package codec

import (
	"fmt"

	"mediacore/internal/media"
)

// PCMVersion is the PCM adapter revision.
var PCMVersion = Version{Name: "pcm", Major: 1, Minor: 0}

// AACVersion is the AAC metadata adapter revision.
var AACVersion = Version{Name: "aac", Major: 1, Minor: 0}

// PCM handles little- (sowt) and big-endian (twos) signed 16-bit PCM, and
// lpcm entries whose flags request the same layout.
type PCM struct{}

// Version implements Adapter.
func (*PCM) Version() Version { return PCMVersion }

// Supports implements Adapter.
func (*PCM) Supports(tag string, _ []byte) bool {
	switch tag {
	case "sowt", "twos", "lpcm", "raw ", "NONE":
		return true
	}
	return false
}

// Describe implements Adapter.
func (*PCM) Describe(tag string, _ []byte, rate, channels, bits int) (Description, error) {
	m := MappingS16LE
	switch tag {
	case "twos":
		m = MappingS16BE
	}
	if bits != 0 && bits != 16 {
		return Description{}, media.NewError(media.CodeUnsupportedCodec, -1, 0,
			"PCM sample width %d bits is not supported (need 16)", bits)
	}
	if channels != 1 && channels != 2 {
		return Description{}, media.NewError(media.CodeChannelsInvalid, -1, 0,
			"PCM channel count %d is not supported (need 1 or 2)", channels)
	}
	if rate <= 0 {
		return Description{}, media.NewError(media.CodeInvalidSampleRate, -1, 0,
			"PCM sample rate %d is invalid", rate)
	}
	f := Format{
		Codec:         "pcm",
		Mapping:       m,
		SampleRate:    rate,
		Channels:      channels,
		BitsPerSample: 16,
		FrameBytes:    2 * channels,
	}
	return Description{Format: f, Adapter: PCMVersion}, nil
}

// Decode implements Adapter.
func (*PCM) Decode(sample []byte, f Format, dst []float64) (int, error) {
	if f.FrameBytes <= 0 {
		return 0, media.NewError(media.CodeUnalignedPCM, -1, 0, "PCM frame size is zero")
	}
	if len(sample)%f.FrameBytes != 0 {
		return 0, media.NewError(media.CodeUnalignedPCM, -1, 0,
			"sample of %d bytes is not a whole number of %d-byte frames", len(sample), f.FrameBytes)
	}
	frames := len(sample) / f.FrameBytes
	if len(dst) < frames*f.Channels {
		return 0, media.NewError(media.CodeMemoryLimit, -1, 0,
			"decode buffer %d < %d values", len(dst), frames*f.Channels)
	}
	be := f.Mapping == MappingS16BE
	for i := 0; i < frames*f.Channels; i++ {
		lo, hi := sample[2*i], sample[2*i+1]
		var v int16
		if be {
			v = int16(uint16(lo)<<8 | uint16(hi))
		} else {
			v = int16(uint16(hi)<<8 | uint16(lo))
		}
		dst[i] = float64(v) / 32768.0
	}
	return frames, nil
}

// AAC recognises mp4a sample entries and parses the AudioSpecificConfig.
type AAC struct{}

// Version implements Adapter.
func (*AAC) Version() Version { return AACVersion }

// Supports implements Adapter.
func (*AAC) Supports(tag string, _ []byte) bool { return tag == "mp4a" || tag == "enca" }

// Describe implements Adapter.
func (*AAC) Describe(tag string, config []byte, rate, channels, _ int) (Description, error) {
	if len(config) < 2 {
		return Description{}, media.NewError(media.CodeInvalidConfig, -1, 0,
			"AAC AudioSpecificConfig missing or shorter than 2 bytes")
	}
	aot := int(config[0] >> 3)
	if aot == 31 {
		return Description{}, media.NewError(media.CodeUnsupportedCodec, -1, 0,
			"AAC object type uses escape form, unsupported")
	}
	if aot != 2 {
		return Description{}, media.NewError(media.CodeUnsupportedCodec, -1, 0,
			"AAC object type %d is not AAC-LC (2)", aot)
	}
	br := bitReader{b: config, bit: 5}
	// aot was consumed: 5 bits.
	srIndex, ok := br.read(4)
	if !ok {
		return Description{}, media.NewError(media.CodeInvalidConfig, -1, 0, "AAC ASC truncated at sample rate")
	}
	sf, okRate := aacRate(int(srIndex))
	if !okRate {
		if srIndex != 0x0f {
			return Description{}, media.NewError(media.CodeInvalidConfig, -1, 0,
				"AAC sampling frequency index %d is reserved", srIndex)
		}
		explicit, ok := br.read(24)
		if !ok {
			return Description{}, media.NewError(media.CodeInvalidConfig, -1, 0, "AAC ASC truncated at explicit rate")
		}
		sf = int(explicit)
	}
	chCfg, ok := br.read(4)
	if !ok {
		return Description{}, media.NewError(media.CodeInvalidConfig, -1, 0, "AAC ASC truncated at channel config")
	}
	ch, okCh := aacChannels(int(chCfg))
	if !okCh {
		if chCfg == 0 {
			return Description{}, media.NewError(media.CodeUnsupportedCodec, -1, 0,
				"AAC program-config-element channel layout is unsupported")
		}
		return Description{}, media.NewError(media.CodeChannelsInvalid, -1, 0,
			"AAC channel configuration %d is invalid", chCfg)
	}
	if sf <= 0 {
		return Description{}, media.NewError(media.CodeInvalidSampleRate, -1, 0, "AAC sample rate %d is invalid", sf)
	}
	// Prefer the ASC rate; the sample-entry rate is often a stale 44100.
	if rate != 0 && rate != sf {
		return Description{
			Format: Format{
				Codec: "aac", Mapping: MappingCompressed, SampleRate: sf,
				Channels: ch, BitsPerSample: 16, ObjectType: aot,
				Config: append([]byte(nil), config...),
			},
			Adapter:  AACVersion,
			Warnings: []string{fmt.Sprintf("sample entry rate %d differs from ASC rate %d", rate, sf)},
		}, nil
	}
	return Description{
		Format: Format{
			Codec: "aac", Mapping: MappingCompressed, SampleRate: sf,
			Channels: ch, BitsPerSample: 16, ObjectType: aot,
			Config: append([]byte(nil), config...),
		},
		Adapter: AACVersion,
	}, nil
}

// Decode implements Adapter. AAC frames are not decoded in this build.
func (*AAC) Decode(_ []byte, f Format, _ []float64) (int, error) {
	return 0, media.NewError(media.CodeUnsupportedCodec, -1, 0,
		"AAC payload decoding is not supported; re-encode the track to PCM")
}

type bitReader struct {
	b   []byte
	bit int
}

func (r *bitReader) read(n int) (int, bool) {
	if r.bit+n > len(r.b)*8 {
		return 0, false
	}
	v := 0
	for i := 0; i < n; i++ {
		byteIdx := (r.bit + i) / 8
		bitIdx := 7 - (r.bit+i)%8
		v = v<<1 | int(r.b[byteIdx]>>uint(bitIdx)&1)
	}
	r.bit += n
	return v, true
}

var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func aacRate(index int) (int, bool) {
	if index < 0 || index >= len(aacRates) {
		return 0, false
	}
	return aacRates[index], true
}

func aacChannels(cfg int) (int, bool) {
	switch cfg {
	case 1, 2, 3, 4, 5, 6, 7:
		// 1..6 are mono..5.1, 7 is 7.1(8ch); for stereo/mono work we
		// accept the well-defined single and paired layouts only.
		if cfg <= 2 {
			return cfg, true
		}
		return 0, false
	}
	return 0, false
}

package mp4

import (
	"io"

	goMp4 "github.com/abema/go-mp4"

	"medpipe/internal/clock"
)

// fillAudioEntry reads the mp4a sample entry for the exact sample rate
// (16.16 fixed point) and channel count, plus the stss sync table.
func fillAudioEntry(r io.ReadSeeker, t *Track) error {
	trakInfo, err := trakByID(r, t.ID)
	if err != nil {
		return err
	}
	payload, err := goMp4.ExtractBoxesWithPayload(r, trakInfo, []goMp4.BoxPath{
		{goMp4.BoxTypeMdia(), goMp4.BoxTypeMinf(), goMp4.BoxTypeStbl(),
			goMp4.BoxTypeStsd(), goMp4.BoxTypeMp4a()},
		{goMp4.BoxTypeMdia(), goMp4.BoxTypeMinf(), goMp4.BoxTypeStbl(), goMp4.BoxTypeStss()},
	})
	if err != nil {
		return wrapParse(err)
	}
	for _, p := range payload {
		switch box := p.Payload.(type) {
		case *goMp4.AudioSampleEntry:
			t.Channels, t.SampleRate = decodeAudioEntry(r, p.Info, box)
		case *goMp4.Stss:
			markSync(t, box.SampleNumber)
		}
	}
	if t.SampleRate == 0 {
		return clock.E(clock.KindUnsupported, "mp4.track", clock.WithTrack(t.ID),
			clock.WithDetail("audio sample entry missing or zero sample rate"))
	}
	return nil
}

// decodeAudioEntry handles both ISO mp4a entries (channel/rate in the fixed
// fields) and QuickTime v2 entries whose fixed fields are placeholders. The
// authoritative fallback for the latter is the AudioSpecificConfig carried
// in the esds descriptor, which encodes channel count and sample rate.
func decodeAudioEntry(r io.ReadSeeker, info goMp4.BoxInfo, ase *goMp4.AudioSampleEntry) (channels uint16, rate uint32) {
	channels, rate = ase.ChannelCount, ase.SampleRate>>16
	if channels != 0 && rate != 0 && ase.EntryVersion == 0 {
		return channels, rate
	}
	payloadSize := info.Size - info.HeaderSize
	raw := make([]byte, payloadSize)
	if _, err := info.SeekToPayload(r); err != nil {
		return channels, rate
	}
	if _, err := io.ReadFull(r, raw); err != nil {
		return channels, rate
	}
	if asc, ok := extractASC(raw); ok {
		if ch, hz, ok := parseASC(asc); ok {
			if ch != 0 {
				channels = ch
			}
			if hz != 0 {
				rate = hz
			}
		}
	}
	return channels, rate
}

// extractASC locates the AudioSpecificConfig inside an esds box payload. The
// ASC is the final tag-0x05 descriptor's data, typically two bytes for AAC
// LC. Descriptor sizes use a 7-bit-per-byte variable encoding.
func extractASC(raw []byte) ([]byte, bool) {
	idx := indexTag(raw, "esds")
	if idx < 0 || idx+8 > len(raw) {
		return nil, false
	}
	// The esds box is a FullBox: 4 version/flags bytes follow its header.
	payload := raw[idx+4+4:]
	asc, ok := findASC(payload)
	if !ok {
		return nil, false
	}
	return asc, true
}

// findASC recursively walks ES/DecoderConfig descriptors and returns the
// DecoderSpecificInfo (tag 0x05) payload containing the AudioSpecificConfig.
func findASC(b []byte) ([]byte, bool) {
	pos := 0
	for pos+2 <= len(b) {
		tag := b[pos]
		size, n := readVarSize(b[pos+1:])
		start := pos + 1 + n
		if start+size > len(b) {
			return nil, false
		}
		body := b[start : start+size]
		if tag == 0x05 {
			return body, true
		}
		// Container descriptors hold nested descriptors after a fixed
		// header: ES_Descriptor (3 bytes: ES_ID 2 + flags 1),
		// DecoderConfigDescriptor (13 bytes: OTI 1 + streamType 1 +
		// bufferSize 3 + maxBitrate 4 + avgBitrate 4).
		if tag == 0x03 || tag == 0x04 {
			headerSkip := 3
			if tag == 0x04 {
				headerSkip = 13
			}
			if headerSkip < len(body) {
				if asc, ok := findASC(body[headerSkip:]); ok {
					return asc, true
				}
			}
		}
		pos = start + size
	}
	return nil, false
}

func readVarSize(b []byte) (int, int) {
	var size, n int
	for n < len(b) && n < 4 {
		cur := b[n]
		size = size<<7 | int(cur&0x7f)
		n++
		if cur&0x80 == 0 {
			break
		}
	}
	return size, n
}

// aacSampleRates is the frequency table index 0..12 of ISO 14496-3.
var aacSampleRates = []uint32{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350,
}

// parseASC decodes the leading fields of an AAC AudioSpecificConfig:
// audioObjectType(5), samplingFrequencyIndex(4; 0xf -> 24-bit explicit),
// channelConfiguration(4).
func parseASC(asc []byte) (channels uint16, rate uint32, ok bool) {
	if len(asc) < 2 {
		return 0, 0, false
	}
	br := bitReader{data: asc}
	objType := br.bits(5)
	if objType == 31 {
		br.bits(6)
	}
	freqIdx := br.bits(4)
	if freqIdx == 0xf {
		rate = uint32(br.bits(24))
	} else if int(freqIdx) < len(aacSampleRates) {
		rate = aacSampleRates[freqIdx]
	}
	chCfg := br.bits(4)
	if chCfg == 0 {
		return 0, rate, false // program_config_element layout, unsupported
	}
	return uint16(chCfg), rate, true
}

type bitReader struct {
	data []byte
	pos  int
}

func (b *bitReader) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		byteIdx := b.pos / 8
		bitIdx := 7 - b.pos%8
		if byteIdx >= len(b.data) {
			return v
		}
		v = v<<1 | uint32((b.data[byteIdx]>>bitIdx)&1)
		b.pos++
	}
	return v
}

func indexTag(raw []byte, tag string) int {
	needle := []byte(tag)
	for i := 0; i+4 <= len(raw); i++ {
		if string(raw[i:i+4]) == string(needle) {
			return i
		}
	}
	return -1
}

// fillColor extracts the colr box of the video sample entry.
func fillColor(r io.ReadSeeker, t *Track) error {
	trakInfo, err := trakByID(r, t.ID)
	if err != nil {
		return err
	}
	bips, err := goMp4.ExtractBoxesWithPayload(r, trakInfo, []goMp4.BoxPath{
		{goMp4.BoxTypeMdia(), goMp4.BoxTypeMinf(), goMp4.BoxTypeStbl(),
			goMp4.BoxTypeStsd(), goMp4.BoxTypeAvc1(), goMp4.BoxTypeColr()},
		{goMp4.BoxTypeMdia(), goMp4.BoxTypeMinf(), goMp4.BoxTypeStbl(), goMp4.BoxTypeStss()},
	})
	if err != nil {
		return wrapParse(err)
	}
	for _, bip := range bips {
		switch box := bip.Payload.(type) {
		case *goMp4.Colr:
			t.Color = &ColorMeta{
				ColorType:               string(box.ColourType[:]),
				ColourPrimaries:         box.ColourPrimaries,
				TransferCharacteristics: box.TransferCharacteristics,
				MatrixCoefficients:      box.MatrixCoefficients,
				FullRange:               box.FullRangeFlag,
			}
		case *goMp4.Stss:
			markSync(t, box.SampleNumber)
		}
	}
	return nil
}

// MarkSyncSamples lets the segment layer learn keyframe positions even when
// stss was found through another path.
func MarkSyncSamples(t *Track, numbers []uint32) { markSync(t, numbers) }

func markSync(t *Track, numbers []uint32) {
	if len(numbers) == 0 {
		// No stss: every sample is a sync sample (ISO 14496-12 semantics).
		for i := range t.Samples {
			t.Samples[i].IsSync = true
		}
		return
	}
	set := make(map[uint32]bool, len(numbers))
	for _, n := range numbers {
		set[n] = true
	}
	for i := range t.Samples {
		if set[uint32(i)+1] { // stss numbers are 1-based
			t.Samples[i].IsSync = true
		}
	}
}

// trakByID resolves the trak box whose tkhd.TrackID equals trackID. Probe
// ids come from tkhd, so trak ordinal position is not reliable.
func trakByID(r io.ReadSeeker, trackID uint32) (*goMp4.BoxInfo, error) {
	traks, err := goMp4.ExtractBoxesWithPayload(r, nil, []goMp4.BoxPath{
		{goMp4.BoxTypeMoov(), goMp4.BoxTypeTrak()},
	})
	if err != nil {
		return nil, wrapParse(err)
	}
	for _, bip := range traks {
		if bip.Info.Type != goMp4.BoxTypeTrak() {
			continue
		}
		info := bip.Info
		tkhds, err := goMp4.ExtractBoxesWithPayload(r, &info, []goMp4.BoxPath{
			{goMp4.BoxTypeTkhd()},
		})
		if err != nil {
			return nil, wrapParse(err)
		}
		for _, tk := range tkhds {
			if tkhd, ok := tk.Payload.(*goMp4.Tkhd); ok && tkhd.TrackID == trackID {
				return &info, nil
			}
		}
	}
	return nil, clock.E(clock.KindCorrupt, "mp4.track", clock.WithTrack(trackID),
		clock.WithDetail("trak/tkhd box not found"))
}

func wrapParse(err error) error {
	if pe, ok := err.(*clock.Error); ok {
		return pe
	}
	return clock.E(clock.KindCorrupt, "mp4.track", clock.WithWrap(err))
}

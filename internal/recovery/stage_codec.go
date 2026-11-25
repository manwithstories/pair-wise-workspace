package recovery

import (
	"mediad/internal/codec"
	"mediad/internal/fault"
	"mediad/internal/manifest"
)

func (e *exec) stageCodec() error {
	adapter := codec.NewV1()
	session, err := adapter.Open(manifest.CodecDescriptor{
		Path: e.hls.Path, Container: "fmp4",
		VideoCodec: "avc1", AudioCodec: "ipcm",
		AdapterVersion: codec.AdapterVersion, SpecVersion: codec.SupportedSpecVersion,
	})
	if err != nil {
		return err
	}

	for _, seg := range e.segs {
		mdatBoxes := mdatBoxesOf(seg.fragment)
		if len(mdatBoxes) == 0 || len(seg.fragment.Mdats) == 0 {
			return fault.New(fault.InvalidManifest, seg.path, int64(seg.index), "missing mdat payload")
		}
		videoBox := mdatBoxes[0]
		nals, err := codec.ScanAVCC(seg.path, seg.fragment.Mdats[0], videoBox.PayloadOffset)
		if err != nil {
			return err
		}
		seg.nals = nals

		audioPayload := seg.fragment.Mdats[len(seg.fragment.Mdats)-1]
		pcm, err := session.DecodePCM16LE(seg.path, audioPayload)
		if err != nil {
			return err
		}
		seg.pcm = pcm
	}

	const blocks = 12
	const blockSamples = 512
	matched := 0
	source := e.segs[0].pcm
	for b := 0; b < blocks; b++ {
		start := b * blockSamples
		if start+blockSamples > len(source) {
			return fault.New(fault.MdatCorrupt, e.segs[0].path, int64(start), "PCM roundtrip block out of range")
		}
		ok, err := session.RoundtripBlock(source[start : start+blockSamples])
		if err != nil {
			return err
		}
		if ok {
			matched++
		}
	}
	e.sum.RoundtripMatched = matched
	e.sum.RoundtripTotal = blocks
	e.sum.RoundtripRate = float64(matched) / float64(blocks)
	return e.finishStage("codec")
}

func mdatBoxesOf(f *manifest.ParsedFragment) []manifest.Box {
	var out []manifest.Box
	for _, b := range f.Boxes {
		if b.Type == "mdat" {
			out = append(out, b)
		}
	}
	return out
}

package remux

import (
	"bytes"
	"os"

	"github.com/disintegration/imaging"

	"relay/internal/demux"
	"relay/internal/media"
)

// VerifyReport 是独立重读输出后的核验结论。
type VerifyReport struct {
	Frames     int
	Tracks     map[uint32]int
	PayloadOK  int
	ImageOK    int
	ManifestOK bool
}

// magic 头用于确认载荷编码与声明一致。
var (
	pngMagic  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// ValidateImage 用 imaging 解码载荷并核对编码魔数与尺寸；不重编码、不落盘。
func ValidateImage(f *media.Frame) error {
	switch f.Codec {
	case media.CodecPNG:
		if !bytes.HasPrefix(f.Payload, pngMagic) {
			return media.NewFault(media.StagePayload, f.SourceOffset, f.CameraID, f.Sequence,
				"declared png payload has wrong magic")
		}
	case media.CodecJPEG:
		if !bytes.HasPrefix(f.Payload, jpegMagic) {
			return media.NewFault(media.StagePayload, f.SourceOffset, f.CameraID, f.Sequence,
				"declared jpeg payload has wrong magic")
		}
	default:
		return media.NewFault(media.StagePayload, f.SourceOffset, f.CameraID, f.Sequence,
			"unknown codec %d", f.Codec)
	}
	img, err := imaging.Decode(bytes.NewReader(f.Payload))
	if err != nil {
		return media.NewFault(media.StagePayload, f.SourceOffset, f.CameraID, f.Sequence,
			"image decode failed: %v", err)
	}
	if f.Width != 0 || f.Height != 0 {
		b := img.Bounds()
		if uint32(b.Dx()) != f.Width || uint32(b.Dy()) != f.Height {
			return media.NewFault(media.StagePayload, f.SourceOffset, f.CameraID, f.Sequence,
				"dimension mismatch: header %dx%d, decoded %dx%d",
				f.Width, f.Height, b.Dx(), b.Dy())
		}
	}
	return nil
}

// VerifyOutput 独立重读输出文件，逐帧核对帧数、轨道、载荷哈希与图像有效性。
// manifest 提供期望清单；任一不符即返回携带坐标的错误。
func VerifyOutput(path string, manifest *Manifest) (*VerifyReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewFault(media.StageConfig, 0, 0, 0, "open for verify: %v", err)
	}
	defer f.Close()

	frames, err := demux.OpenContainer(f, path)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{Tracks: make(map[uint32]int)}
	if len(frames) != len(manifest.Entries) {
		return rep, media.NewFault(media.StageCheckpoint, 0, 0, 0,
			"frame count mismatch: output %d, manifest %d", len(frames), len(manifest.Entries))
	}
	for i, fr := range frames {
		want := manifest.Entries[i]
		if fr.CameraID != want.CameraID || fr.Sequence != want.Sequence {
			return rep, media.NewFault(media.StageCheckpoint, fr.SourceOffset, fr.CameraID, fr.Sequence,
				"entry %d identity mismatch: manifest cam=%d seq=%d", i, want.CameraID, want.Sequence)
		}
		sum := media.SumSHA256(fr.Payload)
		gotHex := hexEncode(sum[:])
		if gotHex != want.PayloadSHA256 {
			return rep, media.NewFault(media.StageSHA, fr.SourceOffset, fr.CameraID, fr.Sequence,
				"payload hash mismatch at entry %d", i)
		}
		if err := ValidateImage(fr); err != nil {
			return rep, err
		}
		rep.PayloadOK++
		rep.ImageOK++
		rep.Tracks[want.TrackID]++
	}
	rep.Frames = len(frames)
	rep.ManifestOK = true
	return rep, nil
}

// hexEncode 是本地小工具，避免在热点路径引入 fmt/encoding 依赖分散。
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0xF]
	}
	return string(out)
}

// Package fixture 生成自包含、确定性的本地样例数据，不引用任何外部素材。
// 相同参数重复运行产生字节一致的文件，保证摘要可复现。
package fixture

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"

	"github.com/disintegration/imaging"

	"relay/internal/media"
)

// 样例几何与节奏常量。
const (
	ImageW      = 8
	ImageH      = 8
	FrameStepNS = int64(33 * media.Millisecond) // ~30fps
	CameraCount = 4
	// 各机位现场时钟相对纪元的固定偏移（map.json 用相反量校正）。
	cameraBaseNS = int64(7 * media.Millisecond)
)

// rngSeed 固定随机种子：图案随机但每次运行完全一致。
const rngSeed = 0x5eed1234

// newPNG 按机位与序号生成确定图案的真实 PNG 载荷，可被 imaging 解码。
func newPNG(cam uint16, seq uint64) []byte {
	r := rand.New(rand.NewSource(int64(cam)*1_000_003 + int64(seq)))
	img := image.NewNRGBA(image.Rect(0, 0, ImageW, ImageH))
	base := color.NRGBA{
		R: uint8(r.Intn(256)), G: uint8(r.Intn(256)),
		B: uint8(r.Intn(256)), A: 255,
	}
	for y := 0; y < ImageH; y++ {
		for x := 0; x < ImageW; x++ {
			c := base
			// 随坐标做确定性明暗变化，使相邻帧载荷不同但可复现。
			c.R = uint8(int(c.R) + x*7 + y*3)
			c.G = uint8(int(c.G) + int(seq&0xFF))
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		panic(err) // 固定输入下编码不会失败
	}
	return buf.Bytes()
}

// makeFrame 构造一条带正确校验的帧（事件时间含机位时钟偏移）。
func makeFrame(cam uint16, seq uint64) *media.Frame {
	payload := newPNG(cam, seq)
	eventNS := media.TimeNS(int64(seq)*FrameStepNS + int64(cam)*cameraBaseNS)
	f := &media.Frame{
		Version: media.WireVersion1, Codec: media.CodecPNG,
		CameraID: cam, Sequence: seq, EventNS: eventNS,
		Width: ImageW, Height: ImageH, Payload: payload,
	}
	rec := media.EncodeRecord(f)
	f.SHA256 = media.SumSHA256(payload)
	f.CRC32C = media.CRC32C(rec[:media.HeaderLen+len(payload)])
	return f
}

// baseFrames 生成每机位 perCam 帧的规则序列（cam 1..CameraCount）。
func baseFrames(perCam int) []*media.Frame {
	out := make([]*media.Frame, 0, perCam*CameraCount)
	for seq := 0; seq < perCam; seq++ {
		for cam := uint16(1); cam <= CameraCount; cam++ {
			out = append(out, makeFrame(cam, uint64(seq)))
		}
	}
	return out
}

package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"review/internal/fragment"
)

func mpdText(camID string) string {
	s := `<?xml version="1.0"?>
<MPD mediaPresentationDuration="PT1.333S"><Period>
<AdaptationSet mimeType="video/mp4">`
	for i, rid := range RepIDs {
		repID := fmt.Sprintf("%s-v-%s", camID, rid)
		s += fmt.Sprintf(`
  <Representation id="%s" bandwidth="%d" codecs="jpeg">
    <SegmentTemplate timescale="90000" initialization="init-%s.mp4"
      media="seg-%s-$Number%%05d$.m4s" startNumber="1" duration="30000" segmentCount="4"/>
  </Representation>`, repID, repBandwidth[i], repID, repID)
	}
	aID := camID + "-a"
	s += fmt.Sprintf(`
</AdaptationSet>
<AdaptationSet mimeType="audio/mp4">
  <Representation id="%s" bandwidth="128000" codecs="lpcm">
    <SegmentTemplate timescale="48000" initialization="init-%s.mp4"
      media="seg-%s-$Number%%05d$.m4s" startNumber="1" duration="16000" segmentCount="4"/>
  </Representation>
</AdaptationSet>
</Period></MPD>
`, aID, aID, aID)
	return s
}

// hlsBaseTimes 四处分片的 tfdt：两处 discontinuity 后时间戳回退归零。
var hlsBaseTimes = []int64{0, 0, FramesPerSeg * VideoFrameDur, 0}

func generateHLS(dir string, l *Layout) error {
	hdir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(hdir, 0o755); err != nil {
		return err
	}
	init := &fragment.InitInfo{Codec: "jpeg", Timescale: VideoTimescale,
		Width: FrameW, Height: FrameH, FrameDuration: VideoFrameDur}
	ip := filepath.Join(hdir, "init-hls.mp4")
	if err := fragment.WriteInit(ip, init); err != nil {
		return err
	}
	l.HLS.InitPath = ip
	m3u8 := "#EXTM3U\n#EXT-X-TIMESCALE:90000\n#EXT-X-MAP:URI=\"init-hls.mp4\"\n"
	for s := 0; s < SegmentsPerRep; s++ {
		var payloads [][]byte
		for f := 0; f < FramesPerSeg; f++ {
			p, err := VideoPayload(9, s*FramesPerSeg+f, 70)
			if err != nil {
				return err
			}
			payloads = append(payloads, p)
		}
		name := fmt.Sprintf("hls-%05d.m4s", s+1)
		sp := filepath.Join(hdir, name)
		if _, err := fragment.WriteSegment(sp, hlsBaseTimes[s], payloads, VideoFrameDur, 0); err != nil {
			return err
		}
		l.HLS.SegPaths = append(l.HLS.SegPaths, sp)
		if s == 1 || s == 3 { // 两处 discontinuity
			m3u8 += "#EXT-X-DISCONTINUITY\n"
		}
		m3u8 += fmt.Sprintf("#EXTINF:0.333333,\n%s\n", name)
	}
	m3u8 += "#EXT-X-ENDLIST\n"
	l.HLS.M3U8Path = filepath.Join(hdir, "index.m3u8")
	return os.WriteFile(l.HLS.M3U8Path, []byte(m3u8), 0o644)
}

// generateCorrupt 生成 CRC 损坏夹具：复制 camA hi 首分片并翻转负载字节。
func generateCorrupt(l *Layout) error {
	fdir := filepath.Join(l.Dir, "fixtures")
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		return err
	}
	src := l.Cameras[0].VideoSegs[0][0]
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	idx := bytes.Index(raw, []byte("mdat"))
	if idx < 0 || idx+20 >= len(raw) {
		return fmt.Errorf("fixture: mdat not found in %s", src)
	}
	raw[idx+16] ^= 0xFF // 翻转 mdat 内首样本字节 → CRC 失配
	l.CorruptPath = filepath.Join(fdir, "corrupt-crc.m4s")
	return os.WriteFile(l.CorruptPath, raw, 0o644)
}

// ApplyTruncateTail 尾部截断故障：截短 camB hi 档末分片。
func (l *Layout) ApplyTruncateTail() (string, error) {
	p := l.Cameras[1].VideoSegs[0][SegmentsPerRep-1]
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	return p, os.Truncate(p, info.Size()-64)
}

// ApplyDropIndex 索引缺项故障：重写 camC hi 档第二分片的 sidx 计数。
func (l *Layout) ApplyDropIndex(n int) (string, error) {
	p := l.Cameras[2].VideoSegs[0][1]
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	idx := bytes.Index(raw, []byte("sidx"))
	if idx < 0 || idx+8 >= len(raw) {
		return "", fmt.Errorf("fixture: sidx not found in %s", p)
	}
	cnt := binary.BigEndian.Uint32(raw[idx+4:])
	if n < 1 || uint32(n) > cnt {
		return "", fmt.Errorf("fixture: invalid drop count %d (have %d)", n, cnt)
	}
	binary.BigEndian.PutUint32(raw[idx+4:], cnt-uint32(n))
	return p, os.WriteFile(p, raw, 0o644)
}

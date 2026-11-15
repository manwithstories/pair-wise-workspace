// Package fixture 按种子确定性生成分片夹具与 ladder.json。
package fixture

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"dashpack/internal/errs"
	"dashpack/internal/source"
)

// Options 是 gen-fixtures 的参数。
type Options struct {
	Out      string
	Reps     int // 总路数（含一路音频）
	Segments int
	Seed     int64
}

const (
	nominalMs     = 2000 // 单片标称时长
	firstOffsetMs = 37   // 注入的首片偏移
	gapIndex      = 9    // 注入的缺片序号
)

// Generate 生成 N-1 路视频 + 1 路音频夹具，并注入三类异常：
// 首片 +37ms、缺第 9 片、末片半时长；GOP 1s/2s 混用作为元数据。
func Generate(o Options) error {
	if o.Reps < 2 {
		return errs.New(errs.ESource, errs.ExitUsage, "reps must be >= 2")
	}
	if o.Segments < 2 {
		return errs.New(errs.ESource, errs.ExitUsage, "segments must be >= 2")
	}
	rng := rand.New(rand.NewSource(o.Seed))
	gapRepIdx, gapAt := 1, int64(gapIndex)
	if o.Reps < 3 {
		gapRepIdx = 0
	}
	if int64(o.Segments) < gapAt {
		gapAt = int64(o.Segments)
	}

	ladder := source.Ladder{Seed: o.Seed}
	for i := 0; i < o.Reps; i++ {
		isAudio := i == o.Reps-1
		rep := source.LadderRep{NominalMs: nominalMs}
		if isAudio {
			rep.ID = "a1"
			rep.ContentType = "audio"
			rep.Bandwidth = 128000
			rep.Codecs = "mp4a.40.2"
			rep.Timescale = 48000
			rep.GopMs = nominalMs
		} else {
			rep.ID = fmt.Sprintf("v%d", i+1)
			rep.ContentType = "video"
			rep.Bandwidth = int64(800000 * (o.Reps - 1 - i))
			rep.Codecs = "avc1.64001f"
			rep.Timescale = 90000
			// GOP 1s 与 2s 混用（仅元数据，不改变片长）。
			if (o.Seed+int64(i))%2 == 0 {
				rep.GopMs = 1000
			} else {
				rep.GopMs = 2000
			}
		}
		rep.InitFile = "init.mp4"
		rep.MediaTemplate = rep.ID + "/seg_$Number%05d$.m4s"
		rep.InitTemplate = rep.ID + "/init.mp4"

		dir := filepath.Join(o.Out, rep.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return errs.New(errs.EWrite, errs.ExitWrite, "mkdir %s: %v", dir, err)
		}
		if err := writeBytes(filepath.Join(dir, rep.InitFile), blob(rng, 64)); err != nil {
			return err
		}
		for n := 1; n <= o.Segments; n++ {
			seg := source.LadderSegment{
				Index:      int64(n),
				File:       fmt.Sprintf("seg_%05d.m4s", n),
				DurationMs: nominalMs,
			}
			if i == 0 && n == 1 {
				seg.OffsetMs = firstOffsetMs // 注入：首片 +37ms
			}
			if isAudio && n == o.Segments {
				seg.DurationMs = nominalMs / 2 // 注入：末片半时长
			}
			skip := i == gapRepIdx && !isAudio && int64(n) == gapAt // 注入：缺第 9 片
			if !skip {
				if err := writeBytes(filepath.Join(dir, seg.File),
					blob(rng, 256)); err != nil {
					return err
				}
			}
			rep.Segments = append(rep.Segments, seg)
		}
		ladder.Reps = append(ladder.Reps, rep)
	}

	data, err := json.MarshalIndent(ladder, "", "  ")
	if err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "encode ladder: %v", err)
	}
	tmp := filepath.Join(o.Out, "ladder.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "write ladder: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(o.Out, "ladder.json")); err != nil {
		os.Remove(tmp)
		return errs.New(errs.EWrite, errs.ExitWrite, "rename ladder: %v", err)
	}
	return nil
}

// blob 生成确定性的伪随机分片内容。
func blob(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func writeBytes(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return errs.New(errs.EWrite, errs.ExitWrite, "write %s: %v", path, err)
	}
	return nil
}

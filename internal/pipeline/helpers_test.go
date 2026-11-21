package pipeline

import (
	"encoding/hex"

	"archivecheck/internal/checkpoint"
	"archivecheck/internal/fixture"
)

// seedCheckpoint 写入一个与配置输入/参数摘要匹配的已完成阶段检查点。
func seedCheckpoint(c Config) error {
	inSum := fixture.InputDigest(c.Params)
	pmSum := ParamsHash(c)
	return checkpoint.Save(c.CheckpointPath, checkpoint.State{
		InputDigest:  hex.EncodeToString(inSum[:]),
		ParamsDigest: hex.EncodeToString(pmSum[:]),
		FramesDone:   c.Params.Frames,
		Stage:        "segment",
	})
}

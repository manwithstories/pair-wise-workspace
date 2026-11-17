// repair_pipeline.h — 串联检测、修复、残差计算与结果一致性校验
//
// 数据契约：本流水线只运行一次检测，产出的 RepairResult 同时供 JSON
// 报告与修复音频写盘两条路径使用，禁止任何一侧重复实现算法。
#pragma once

#include <cstddef>
#include <string>
#include <vector>

#include "clip_detector.h"
#include "interpolator.h"
#include "sample_model.h"

namespace popfix {

class RepairPipeline {
 public:
  RepairPipeline(DetectorConfig dcfg, RepairConfig rcfg)
      : detector_(dcfg), rcfg_(rcfg) {}

  // 就地修复 audio（内部先保留原始副本用于残差与峰值对照）。
  // 返回完整 RepairResult；dry_run=true 时不修改任何样本，只做检测与统计。
  RepairResult run(AudioBuffer& audio, bool dry_run);

  // 依据修复后音频与原始副本复算残差能量，供 verify 子命令比对报告。
  static double recompute_residual(const AudioBuffer& original,
                                   const AudioBuffer& repaired,
                                   const std::vector<EventRepair>& repairs);

 private:
  // 构造每帧每声道的削波标记（检测事件覆盖到的样本即视为不可作锚点）。
  static std::vector<std::vector<bool>> build_clipped_mask(
      std::size_t frames, std::uint16_t channels,
      const std::vector<ClipEvent>& events);

  ClipDetector detector_;
  RepairConfig rcfg_;
};

}  // namespace popfix

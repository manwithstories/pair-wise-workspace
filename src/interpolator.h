// interpolator.h — 带边界保护的三阶 Hermite 插值修复
//
// 对一段需要修复的帧区间 [first, last]，使用两侧最近的有效（未削波）
// 样本作为锚点：
//   - 两侧锚点齐全且至少含 4 个有效样本时，使用三阶 Hermite，
//     切线由有效样本的局部中心差分估计并限幅，抑制过冲；
//   - 另一侧仍削波、边界上下文不足时降级为二阶（三点二次）插值；
//   - 有效样本不足 3 个时判定修复失败，由调用方记录 repair_failed。
// 所有降级路径与数值溢出（插值结果越界被钳制）都通过 warnings 返回。
#pragma once

#include <cstddef>
#include <cstdint>
#include <vector>

#include "sample_model.h"

namespace popfix {

// 单声道修复输入。
struct InterpRequest {
  std::size_t first = 0;  // 待替换帧区间（闭区间）
  std::size_t last = 0;
  std::uint8_t channel = 0;
};

// 单声道修复输出。
struct InterpOutcome {
  bool ok = false;
  bool degraded = false;       // 使用了二阶降级
  bool clipped_context = false;  // 锚点撞到文件首尾
  bool overflow = false;       // 插值结果越界被钳制
  std::vector<RepairWarning> warnings;
};

class Interpolator {
 public:
  // audio 同时提供输入与就地写回；clipped[frame][channel] 标记该样本
  // 是否属于削波（不可作为锚点）。warnings 的 event_id 由调用方补全。
  InterpOutcome repair_channel(
      AudioBuffer& audio,
      const std::vector<std::vector<bool>>& clipped,
      const InterpRequest& req) const;

 private:
  static double hermite(double p0, double p1, double m0, double m1,
                        double t);
};

}  // namespace popfix

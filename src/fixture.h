// fixture.h — 样例数据生成：不引用任何外部文件，全部由种子确定性合成
//
// 生成低幅双频正弦 + 弱噪声的"健康"基底（远低于满幅阈值），再注入
// 三个确定性异常事件：
//   事件0  单点爆音（仅左声道，2 帧满幅尖峰）
//   事件1  左右声道不一致的短削波段（仅右声道，48 帧）
//   事件2  长连续削波段（双声道同帧，100 帧）
// 异常帧合计 2 + 48 + 100 = 150，与验收的 repaired_frames≈150 对齐
// （最终统计以实际检测/修复帧数为准）。
#pragma once

#include <cstddef>
#include <cstdint>

#include "sample_model.h"

namespace popfix {

struct FixtureSpec {
  std::uint32_t sample_rate = 48000;
  std::uint16_t channels = 2;
  std::size_t frames = 96000;
  std::uint64_t seed = 17;
};

AudioBuffer generate_fixture(const FixtureSpec& spec);

}  // namespace popfix

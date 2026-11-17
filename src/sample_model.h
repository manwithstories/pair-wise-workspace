// sample_model.h — 交错样本、声道帧、爆音区间、修复结果与统计结构
//
// 所有模块共享的纯数据模型。样本在内存中统一以 double 归一化到 [-1, 1]
// 表示（峰值计算因此不会整数溢出）；整数 <-> double 的转换由 wav_io 负责。
#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace popfix {

// 退出码契约：0 成功；2 参数/输入错误；3 检测或修复失败；
//             4 校验不通过；5 输出文件写入失败
enum class ExitCode : int {
  Ok = 0,
  UsageOrInputError = 2,
  DetectOrRepairFailure = 3,
  VerifyFailure = 4,
  OutputFailure = 5,
};

// 交错 PCM 音频缓冲。sample(frame, channel) = data[frame * channels + ch]。
struct AudioBuffer {
  std::uint32_t sample_rate = 0;
  std::uint16_t channels = 0;
  std::uint16_t bits_per_sample = 0;
  std::vector<double> data;  // 交错、归一化样本

  std::size_t frames() const {
    return channels == 0 ? 0 : data.size() / channels;
  }
  double& sample(std::size_t frame, std::size_t ch) {
    return data[frame * channels + ch];
  }
  double sample(std::size_t frame, std::size_t ch) const {
    return data[frame * channels + ch];
  }
};

// 检测阶段产生的爆音/削波事件。
// start_frame/end_frame 为闭区间；channel_mask 位 0=左/声道0，位 1=右/声道1。
struct ClipEvent {
  std::size_t event_id = 0;
  std::size_t start_frame = 0;
  std::size_t end_frame = 0;      // 闭区间
  std::uint8_t channel_mask = 0;  // 参与事件的声道位掩码
  bool single_sample = false;     // 单点爆音（非连续削波）
};

// 修复阶段降级/异常告警。
struct RepairWarning {
  std::size_t event_id = 0;
  std::uint8_t channel = 0;
  std::string code;  // clipped_context | degraded_quadratic |
                     // other_side_clipped | overflow_clamped |
                     // insufficient_context | repair_failed
  std::string detail;
};

// 单个事件的修复记录。
struct EventRepair {
  std::size_t event_id = 0;
  std::size_t start_frame = 0;   // 检测事件原始区间（闭区间）
  std::size_t end_frame = 0;
  std::uint8_t channel_mask = 0;
  std::size_t repair_start = 0;  // 扩展后的实际修复窗口（闭区间）
  std::size_t repair_end = 0;
  bool clipped_context = false;  // 扩展窗口撞到文件首尾被裁剪
  double residual_energy = 0.0;  // 该事件被替换样本的均方残差
  bool failed = false;
  std::vector<RepairWarning> warnings;
};

// 一次检测/修复运行的汇总统计（JSON 与终端摘要共享同一份结构）。
struct RepairStats {
  std::size_t event_count = 0;
  std::size_t repaired_frames = 0;  // 至少一个声道被修改的帧数
  std::size_t repaired_samples = 0;
  double peak_before = 0.0;
  double peak_after = 0.0;
  double residual_energy = 0.0;  // 全部被替换样本的全局均方残差
};

// 完整修复结果。analyze 的 JSON 报告与写音频路径共享这一个对象，
// 两条路径不允许各自重复实现算法。
struct RepairResult {
  RepairStats stats;
  std::vector<ClipEvent> events;
  std::vector<EventRepair> repairs;
  std::vector<RepairWarning> warnings;
  bool success = false;
};

// 检测器参数。
struct DetectorConfig {
  double threshold = 0.995;  // 满幅归一化阈值
  std::size_t merge_gap = 2; // 同声道相邻段间隔 <= 该值时合并为一个事件
};

// 修复器参数。
struct RepairConfig {
  std::size_t context = 32;  // 修复区间向每侧外扩的最小样本数
  unsigned threads = 1;      // 修复线程数（互不重叠的事件可并行）
};

}  // namespace popfix

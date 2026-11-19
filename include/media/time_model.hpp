#pragma once
// 统一时间基准、漂移参数与错误类型 —— 修复、索引、报告模块共用本文件。
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace media {

// 统一时间基：微秒，1 秒 = 1_000_000 us。所有轨道修复后都映射到该基准。
inline constexpr int64_t UNIFIED_TB_NUM = 1;
inline constexpr int64_t UNIFIED_TB_DEN = 1'000'000;

// 33 位 PTS 回绕掩码（MPEG-TS 风格的 33 位时间戳）。
inline constexpr uint64_t PTS33_MASK = (uint64_t(1) << 33) - 1;
inline constexpr uint64_t PTS33_MOD = uint64_t(1) << 33;

// 有理数时间基（num/den，单位秒/滴答）。
struct Rational {
  int64_t num = 1;
  int64_t den = 1;
};

// 滴答 -> 统一微秒（__int128 防止长素材乘法溢出）。
inline int64_t ticksToUs(int64_t ticks, Rational tb) {
  return int64_t((__int128_t)ticks * tb.num * UNIFIED_TB_DEN / tb.den);
}
// 统一微秒 -> 滴答。
inline int64_t usToTicks(int64_t us, Rational tb) {
  return int64_t((__int128_t)us * tb.den / (tb.num * UNIFIED_TB_DEN));
}
inline double usToSeconds(int64_t us) { return double(us) / 1e6; }
inline double usToMs(int64_t us) { return double(us) / 1e3; }

// 全流程错误分类。可恢复故障写诊断继续；致命故障令 CLI 返回 2。
enum class TimeError {
  None,
  Wrap,                 // 检测到 33 位回绕（可恢复）
  BackwardIllegal,      // 非法倒退，无法安全修复（致命/区段不可交付）
  DuplicatePts,         // 时间戳重复（可恢复，保留并标记）
  Ambiguous,            // 回绕/倒退歧义，无法安全修复（致命）
  CorruptPacket,        // 损坏包（可恢复，跳过/记录）
  TruncatedInput,       // 截断输入（致命）
  ZeroDuration,         // 零时长轨道（致命）
  MissingAnchor,        // 缺少漂移锚点（置信度不足，可恢复）
  QueueCancelled,       // 队列被下游错误取消（致命向上游传播）
  IoError               // 索引/文件 IO 错误（致命）
};

inline bool isFatal(TimeError e) {
  switch (e) {
  case TimeError::BackwardIllegal:
  case TimeError::Ambiguous:
  case TimeError::TruncatedInput:
  case TimeError::ZeroDuration:
  case TimeError::QueueCancelled:
  case TimeError::IoError:
    return true;
  default:
    return false;
  }
}

const char* toString(TimeError e);

// 漂移参数：修复后时间 = 斜率 * 原始时间 + 偏移，斜率以 ppm 表达。
struct DriftParams {
  double slope = 1.0;     // 无量纲，1.0 = 无漂移
  int64_t offsetUs = 0;   // 常数时钟偏移
  double driftPpm = 0.0;  // (slope - 1) * 1e6
  double r2 = 0.0;        // 线性拟合决定系数
  double confidence = 0.0;// 0..1，锚点不足时为低置信度
  bool lowConfidence = true;
};

// 时间线上的可恢复/致命事件，用于机器可读诊断。
struct TimeEvent {
  size_t packetIndex = 0;
  int64_t rawPts = 0;
  int64_t repairedUs = 0;
  TimeError error = TimeError::None;
  std::string detail;
};

// 一元线性拟合 y = a*x + b（漂移估计共用）。
struct LineFit {
  double a = 0.0, b = 0.0;
  double r2 = 0.0;
  size_t points = 0;
  bool valid = false;
};
LineFit fitLine(const std::vector<std::pair<double, double>>& pts);

// 漂移斜率安全钳制：实测晶振漂移通常 < 1000 ppm。
inline constexpr double MAX_SLOPE_PPM = 1000.0;
double clampSlopePpm(double ppm);

} // namespace media

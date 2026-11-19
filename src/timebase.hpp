// 统一时间基准：所有时间一律以整数刻度表示，禁止浮点累积。
// 主时间刻度固定 90 kHz，可整除常见帧率（24/25/30/60 及 30000/1001 系列）。
#pragma once
#include <cstdint>
#include <optional>
#include <string>

namespace mb {

inline constexpr int64_t kMasterHz = 90000;  // 主刻度：每秒 tick 数

// 有理数比率（num/den），用于帧率、时间基，始终精确表示。
struct Ratio {
  int64_t num = 0;
  int64_t den = 1;
  static Ratio reduce(int64_t n, int64_t d);
  bool valid() const { return den > 0 && num > 0; }
};

// 时间码制式：普通（非丢帧）或 SMPTE 丢帧（仅 29.97/59.94 等 NTSC 分数帧率）。
struct Timebase {
  Ratio rate;            // 帧率 num/den
  bool drop_frame = false;
  static Timebase fps(int64_t num, int64_t den, bool df = false);
  static Timebase fps24() { return fps(24, 1); }
  static Timebase fps25() { return fps(25, 1); }
  static Timebase fps30() { return fps(30, 1); }
  static Timebase ntsc30() { return fps(30000, 1001, true); }
  static Timebase ntsc30ndf() { return fps(30000, 1001, false); }
  bool valid() const { return rate.valid(); }
};

// 帧号（该帧率下从 0 起的整数帧）与主刻度互转，全部整数乘除。
int64_t frame_to_master(int64_t frame, const Timebase& tb);
int64_t master_to_frame(int64_t master_ticks, const Timebase& tb);  // 向下取整

// 时间码解析与格式化（HH:MM:SS:FF 或 HH:MM:SS;FF 丢帧；日界回绕）。
// 无法换算（非法数值、丢帧标记与制式不符、跳过的丢帧号）返回 nullopt。
struct TcParts {
  int hh, mm, ss, ff;
};
std::optional<TcParts> parse_timecode_parts(const std::string& s,
                                            bool* drop_mark = nullptr);
std::optional<int64_t> timecode_to_frame(const std::string& s,
                                         const Timebase& tb);
std::string frame_to_timecode(int64_t frame, const Timebase& tb);

// SRT/WebVTT 毫秒时间戳：HH:MM:SS,mmm 或 HH:MM:SS.mmm（VTT 亦可 MM:SS.mmm）。
// 返回主刻度 tick；非法返回 nullopt。
std::optional<int64_t> parse_ms_timestamp(const std::string& s);
std::string format_ms_timestamp(int64_t master_ticks);  // 输出 SRT 风格

// 主刻度与毫秒/微秒互转（精确；毫秒输入按 tick 四舍五入）。
int64_t ms_to_master(int64_t ms);
int64_t master_to_ms(int64_t ticks);
int64_t us_to_master(int64_t us);

}  // namespace mb

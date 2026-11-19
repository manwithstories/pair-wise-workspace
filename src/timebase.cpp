#include "timebase.hpp"

#include <algorithm>
#include <array>
#include <cctype>
#include <cmath>
#include <cstdio>
#include <sstream>

namespace mb {

namespace {
int64_t gcd64(int64_t a, int64_t b) {
  while (b) { int64_t t = a % b; a = b; b = t; }
  return a < 0 ? -a : a;
}
// 丢帧制式下每秒的名义帧数（30000/1001 -> 30）。
int64_t nominal_fps(const Timebase& tb) {
  return (tb.rate.num + tb.rate.den / 2) / tb.rate.den;  // 四舍五入
}
}  // namespace

Ratio Ratio::reduce(int64_t n, int64_t d) {
  if (d <= 0 || n <= 0) return {0, 1};
  int64_t g = gcd64(n, d);
  return {n / g, d / g};
}

Timebase Timebase::fps(int64_t num, int64_t den, bool df) {
  Timebase tb;
  tb.rate = Ratio::reduce(num, den);
  tb.drop_frame = df && tb.rate.num != tb.rate.den * nominal_fps(
                      Timebase{Ratio::reduce(num, den), false});
  return tb;
}

int64_t frame_to_master(int64_t frame, const Timebase& tb) {
  // frame * kMasterHz * den / num，先乘后除避免精度损失。
  return frame * kMasterHz * tb.rate.den / tb.rate.num;
}

int64_t master_to_frame(int64_t ticks, const Timebase& tb) {
  return ticks * tb.rate.num / (kMasterHz * tb.rate.den);
}

std::optional<TcParts> parse_timecode_parts(const std::string& s,
                                            bool* drop_mark) {
  if (drop_mark) *drop_mark = false;
  // 接受 ':' 或 ';' 分隔（; 表示丢帧标记）。
  TcParts p{};
  char c1, c2, c3;
  long hh, mm, ss, ff;
  // 先探测分隔符，再校验长度与字符。
  if (s.size() < 11) return std::nullopt;
  for (size_t i = 0; i < s.size(); ++i) {
    if (i == 2 || i == 5) {
      if (s[i] != ':' && s[i] != ';') return std::nullopt;
    } else if (i == 8) {
      if (s[i] != ':' && s[i] != ';') return std::nullopt;
      if (s[i] == ';') { if (drop_mark) *drop_mark = true; }
    } else if (!std::isdigit(static_cast<unsigned char>(s[i]))) {
      return std::nullopt;
    }
  }
  c1 = s[2]; c2 = s[5]; c3 = s[8];
  (void)c1; (void)c2;
  if (c3 == ';' && drop_mark) *drop_mark = true;
  std::sscanf(s.c_str(), "%ld%*c%ld%*c%ld%*c%ld", &hh, &mm, &ss, &ff);
  p = {static_cast<int>(hh), static_cast<int>(mm),
       static_cast<int>(ss), static_cast<int>(ff)};
  if (p.mm > 59 || p.ss > 59 || p.ff < 0) return std::nullopt;
  return p;
}

std::optional<int64_t> timecode_to_frame(const std::string& s,
                                         const Timebase& tb) {
  if (!tb.valid()) return std::nullopt;
  bool drop_mark = false;
  auto parts = parse_timecode_parts(s, &drop_mark);
  if (!parts) return std::nullopt;
  if (drop_mark != tb.drop_frame) return std::nullopt;  // 标记须与制式一致
  const int64_t f = nominal_fps(tb);
  if (parts->ff >= f) return std::nullopt;
  const int64_t frames_per_day = f * 86400;
  int64_t nominal = (((int64_t)parts->hh * 60 + parts->mm) * 60 + parts->ss) * f
                    + parts->ff;
  if (tb.drop_frame) {
    // 丢帧号在每分钟首（除每 10 分钟）跳过 drop 个帧号；落在跳过号上非法。
    const int64_t drop = f / 15;  // 29.97 -> 2，59.94 -> 4
    int64_t total_min = (int64_t)parts->hh * 60 + parts->mm;
    if (parts->ss == 0 && parts->ff < drop && total_min % 10 != 0)
      return std::nullopt;  // 整 10 分钟边界不跳帧，;00/;01 合法
    int64_t skipped = drop * (total_min - total_min / 10);
    // SMPTE 12M 正解：真实帧号 = 名义帧 − 累计跳过帧数（显示号不含跳过帧）。
    nominal -= skipped;
  }
  // 日界回绕：帧号对一天帧数取模。
  nominal %= frames_per_day;
  if (nominal < 0) nominal += frames_per_day;
  return nominal;
}

std::string frame_to_timecode(int64_t frame, const Timebase& tb) {
  const int64_t f = nominal_fps(tb);
  const int64_t frames_per_day = f * 86400;
  frame %= frames_per_day;
  if (frame < 0) frame += frames_per_day;
  int64_t hh, mm, ss, ff;
  int64_t n = frame;
  if (tb.drop_frame) {
    // 标准 SMPTE 12M 丢帧反解：10 分钟为一个周期。
    const int64_t drop = f / 15;                    // 每分跳过帧数（29.97->2）
    const int64_t cycle = 10 * 60 * f - 9 * drop;   // 每周期实际帧数
    int64_t cycles = n / cycle;
    int64_t r = n % cycle;
    // 周期内已过去的分钟数（首分钟少跳 drop 帧，故先减 drop）。
    int64_t units = (r - drop) / (60 * f - drop);
    n += drop * 9 * cycles + drop * units;          // 还原名义帧计数
  }
  ff = n % f;
  int64_t total_sec = n / f;
  ss = total_sec % 60;
  mm = (total_sec / 60) % 60;
  hh = total_sec / 3600;
  char buf[16];
  char sep = tb.drop_frame ? ';' : ':';
  std::snprintf(buf, sizeof(buf), "%02lld:%02lld:%02lld%c%02lld",
                (long long)hh, (long long)mm, (long long)ss, sep,
                (long long)ff);
  return buf;
}

std::optional<int64_t> parse_ms_timestamp(const std::string& sIn) {
  std::string s = sIn;
  // 去首尾空白；VTT 可用 '.'，SRT 用 ','，统一接受。
  while (!s.empty() && std::isspace((unsigned char)s.back())) s.pop_back();
  size_t start = 0;
  while (start < s.size() && std::isspace((unsigned char)s[start])) ++start;
  s = s.substr(start);
  int64_t hh = 0, mm = 0, ss = 0, ms = 0;
  char c1 = 0, c2 = 0, c3 = 0;
  int consumed = 0;
  // 形态1 HH:MM:SS.mmm / ,mmm；形态2 MM:SS.mmm（VTT）。
  long a = 0, b = 0, c = 0, d = 0;
  int n = std::sscanf(s.c_str(), "%ld:%ld:%ld%c%ld%n", &a, &b, &c, &c3, &d,
                      &consumed);
  if (n >= 5 && (c3 == '.' || c3 == ',')) {
    hh = a; mm = b; ss = c; ms = d;
  } else {
    n = std::sscanf(s.c_str(), "%ld%c%ld%c%ld%n", &a, &c1, &b, &c2, &c,
                    &consumed);
    if (n >= 5 && c1 == ':' && (c2 == '.' || c2 == ',')) {
      mm = a; ss = b; ms = c;
    } else {
      return std::nullopt;
    }
  }
  if (mm > 59 || ss > 59 || ms < 0 || ms > 999) return std::nullopt;
  return ms_to_master(hh * 3600000 + mm * 60000 + ss * 1000 + ms);
}

std::string format_ms_timestamp(int64_t ticks) {
  int64_t total_ms = master_to_ms(ticks);
  int64_t ms = total_ms % 1000;
  int64_t sec = (total_ms / 1000) % 60;
  int64_t min = (total_ms / 60000) % 60;
  int64_t hr = total_ms / 3600000;
  char buf[16];
  std::snprintf(buf, sizeof(buf), "%02lld:%02lld:%02lld,%03lld",
                (long long)hr, (long long)min, (long long)sec, (long long)ms);
  return buf;
}

int64_t ms_to_master(int64_t ms) {
  // 四舍五入到最近 tick（90 kHz 下 1 ms = 90 tick，通常整除）。
  return (ms * kMasterHz + 500) / 1000;
}
int64_t master_to_ms(int64_t ticks) {
  return (ticks * 1000 + kMasterHz / 2) / kMasterHz;
}
int64_t us_to_master(int64_t us) {
  return (us * kMasterHz + 500000) / 1000000;
}

}  // namespace mb

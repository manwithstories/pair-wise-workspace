// clip_detector.cpp — 逐声道扫描、同轨合并、方波过滤、跨声道关联
#include "clip_detector.h"

#include <algorithm>
#include <cmath>

namespace popfix {

namespace {

// 提取一个声道的连续样本（非交错），降低后续索引复杂度。
std::vector<double> extract_channel(const AudioBuffer& audio,
                                    std::uint8_t channel) {
  const std::size_t n = audio.frames();
  std::vector<double> ch(n);
  for (std::size_t i = 0; i < n; ++i) ch[i] = audio.sample(i, channel);
  return ch;
}

}  // namespace

std::vector<RawSpan> ClipDetector::merge_same_channel(
    std::vector<RawSpan> spans, std::size_t gap) {
  if (spans.empty()) return spans;
  std::vector<RawSpan> out;
  out.push_back(spans.front());
  for (std::size_t i = 1; i < spans.size(); ++i) {
    RawSpan& prev = out.back();
    const RawSpan& cur = spans[i];
    const bool close = cur.start >= prev.end &&
                       (cur.start - prev.end) <= (gap + 1);
    // 只合并同一削波轨（同号），避免把正负交替的方波拼成一个事件。
    if (close && cur.positive_rail == prev.positive_rail) {
      prev.end = cur.end;
      prev.single_sample = false;
    } else {
      out.push_back(cur);
    }
  }
  return out;
}

bool ClipDetector::passes_recovery_shape(const std::vector<double>& ch,
                                         const RawSpan& span) const {
  const std::size_t n = ch.size();
  const double thr = cfg_.threshold;

  // 估计局部常态斜率：取区间两侧各 8 个非削波样本的平均绝对差分。
  double local_slope = 0.0;
  int counted = 0;
  auto collect = [&](std::size_t idx) {
    if (idx == 0 || idx >= n) return;
    const double a = ch[idx - 1], b = ch[idx];
    if (std::fabs(a) < thr && std::fabs(b) < thr) {
      local_slope += std::fabs(b - a);
      ++counted;
    }
  };
  for (std::size_t k = 1; k <= 8; ++k) {
    if (span.start >= k) collect(span.start - k);
    if (span.end + k < n) collect(span.end + k);
  }
  local_slope = counted > 0 ? local_slope / counted : 0.0;

  const double rail = span.positive_rail ? 1.0 : -1.0;

  // 单点爆音：两侧相邻样本必须明显低于阈值（向基线恢复的尖峰形态）。
  if (span.single_sample) {
    const bool left_ok =
        span.start == 0 || std::fabs(ch[span.start - 1]) < thr * 0.98;
    const bool right_ok =
        span.end + 1 >= n || std::fabs(ch[span.end + 1]) < thr * 0.98;
    if (!(left_ok && right_ok)) return false;
    // 与邻居的跳变必须显著超过局部常态斜率，否则只是低频波峰。
    const double jump =
        std::fabs(ch[span.start] -
                  (span.start > 0 ? ch[span.start - 1] : 0.0));
    return jump > std::max(0.05, 4.0 * local_slope);
  }

  // 连续削波段：平坦顶部（段内样本贴近同一满幅轨）。
  int flat = 0;
  for (std::size_t i = span.start; i <= span.end; ++i) {
    if (std::fabs(ch[i] - rail) < (1.0 - thr) * 2.0) ++flat;
  }
  if (flat < static_cast<int>(span.end - span.start + 1) - 1) return false;

  // 局部恢复形态：至少一侧存在可测的恢复跳变，且明显陡于常态斜率。
  double recovery = 0.0;
  if (span.start > 0)
    recovery = std::max(recovery, std::fabs(ch[span.start] -
                                            ch[span.start - 1]));
  if (span.end + 1 < n)
    recovery = std::max(recovery,
                        std::fabs(ch[span.end + 1] - ch[span.end]));
  return recovery > std::max(0.02, 3.0 * local_slope);
}

std::vector<RawSpan> ClipDetector::detect_channel(
    const AudioBuffer& audio, std::uint8_t channel) const {
  const std::vector<double> ch = extract_channel(audio, channel);
  const std::size_t n = ch.size();
  const double thr = cfg_.threshold;

  std::vector<RawSpan> raw;
  std::size_t i = 0;
  while (i < n) {
    if (std::fabs(ch[i]) < thr) {
      ++i;
      continue;
    }
    const bool positive = ch[i] > 0.0;
    const std::size_t start = i;
    while (i < n && std::fabs(ch[i]) >= thr &&
           (ch[i] > 0.0) == positive) {
      ++i;
    }
    RawSpan span;
    span.start = start;
    span.end = i - 1;
    span.single_sample = (span.end == span.start);
    span.positive_rail = positive;
    if (passes_recovery_shape(ch, span)) raw.push_back(span);
  }

  return merge_same_channel(std::move(raw), cfg_.merge_gap);
}

std::vector<ClipEvent> ClipDetector::detect(const AudioBuffer& audio) const {
  const std::uint16_t nch = audio.channels;
  std::vector<std::vector<RawSpan>> per_channel(nch);
  for (std::uint8_t c = 0; c < nch; ++c)
    per_channel[c] = detect_channel(audio, c);

  // 以 (start, end) 归并所有声道的段：帧上重叠或紧邻（<= merge_gap）
  // 即视为同一事件，channel_mask 累加来源声道。
  struct Acc {
    std::size_t start, end;
    std::uint8_t mask;
    bool single;
  };
  std::vector<Acc> acc;
  for (std::uint8_t c = 0; c < nch; ++c) {
    for (const RawSpan& s : per_channel[c]) {
      bool merged = false;
      for (Acc& a : acc) {
        const bool overlap = !(s.end < a.start || s.start > a.end);
        const bool adjacent =
            !overlap && (s.start > a.end ? s.start - a.end
                                         : a.start - s.end) <=
                             cfg_.merge_gap + 1;
        if (overlap || adjacent) {
          a.start = std::min(a.start, s.start);
          a.end = std::max(a.end, s.end);
          a.mask = static_cast<std::uint8_t>(a.mask | (1u << c));
          a.single = a.single && s.single_sample;
          merged = true;
          break;
        }
      }
      if (!merged)
        acc.push_back(Acc{s.start, s.end,
                          static_cast<std::uint8_t>(1u << c),
                          s.single_sample});
    }
  }

  std::sort(acc.begin(), acc.end(),
            [](const Acc& a, const Acc& b) {
              return a.start < b.start ||
                     (a.start == b.start && a.end < b.end);
            });

  std::vector<ClipEvent> events;
  events.reserve(acc.size());
  for (std::size_t k = 0; k < acc.size(); ++k) {
    ClipEvent e;
    e.event_id = k;
    e.start_frame = acc[k].start;
    e.end_frame = acc[k].end;
    e.channel_mask = acc[k].mask;
    e.single_sample = acc[k].single;
    events.push_back(e);
  }
  return events;
}

}  // namespace popfix

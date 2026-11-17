// interpolator.cpp — Hermite 三阶 / 二次降级 / 失败判定
#include "interpolator.h"

#include <algorithm>
#include <cmath>

namespace popfix {

namespace {
void warn(std::vector<RepairWarning>& ws, std::uint8_t ch, const char* code,
          const std::string& detail) {
  RepairWarning w;
  w.channel = ch;
  w.code = code;
  w.detail = detail;
  ws.push_back(std::move(w));
}
}  // namespace

double Interpolator::hermite(double p0, double p1, double m0, double m1,
                             double t) {
  const double t2 = t * t;
  const double t3 = t2 * t;
  const double h00 = 2 * t3 - 3 * t2 + 1;
  const double h10 = t3 - 2 * t2 + t;
  const double h01 = -2 * t3 + 3 * t2;
  const double h11 = t3 - t2;
  return h00 * p0 + h10 * m0 + h01 * p1 + h11 * m1;
}

InterpOutcome Interpolator::repair_channel(
    AudioBuffer& audio,
    const std::vector<std::vector<bool>>& clipped,
    const InterpRequest& req) const {
  InterpOutcome out;
  const std::size_t n = audio.frames();
  const std::uint8_t ch = req.channel;

  auto is_clipped = [&](std::size_t f) {
    return f < n && clipped[f][ch];
  };

  // ---- 收集锚点：修复窗口两侧各找最近的若干有效样本 ----------------
  std::vector<std::size_t> left;   // 升序，均 < first
  for (std::size_t k = req.first; k-- > 0 && left.size() < 3;) {
    if (!is_clipped(k)) left.push_back(k);
    if (k == 0) break;
  }
  std::vector<std::size_t> right;  // 升序，均 > last
  for (std::size_t k = req.last + 1; k < n && right.size() < 3; ++k) {
    if (!is_clipped(k)) right.push_back(k);
  }
  std::reverse(left.begin(), left.end());

  if (req.first == 0 || req.last + 1 >= n) {
    out.clipped_context = true;
    warn(out.warnings, ch, "clipped_context",
         "修复窗口触及文件首尾，锚点上下文被裁剪");
  }

  const std::size_t valid = left.size() + right.size();
  if (valid < 3) {
    out.ok = false;
    warn(out.warnings, ch, "insufficient_context",
         "有效锚点不足 3 个，无法插值修复");
    warn(out.warnings, ch, "repair_failed",
         "该声道区间修复失败，保留原始样本");
    return out;
  }

  // 单侧缺失（另一侧仍削波或边界）→ 二阶降级。
  const bool need_quadratic = left.empty() || right.empty() || valid < 4;
  if (need_quadratic) out.degraded = true;

  auto value = [&](std::size_t f) { return audio.sample(f, ch); };

  if (!need_quadratic) {
    // ---- 三阶 Hermite：p0=最近左锚点，p1=最近右锚点 ----------------
    const std::size_t a = left.back();
    const std::size_t b = right.front();
    const double p0 = value(a);
    const double p1 = value(b);
    // 中心差分切线（按锚点间距归一化），再限幅防止过冲。
    double m0 = p1 - p0;
    if (left.size() >= 2)
      m0 = (value(right.front()) - value(left[left.size() - 2])) /
           static_cast<double>(b - left[left.size() - 2]) *
           static_cast<double>(b - a);
    double m1 = m0;
    if (right.size() >= 2)
      m1 = (value(right[1]) - value(a)) /
           static_cast<double>(right[1] - a) *
           static_cast<double>(b - a);
    m0 = std::clamp(m0, -1.0, 1.0);
    m1 = std::clamp(m1, -1.0, 1.0);

    const double span = static_cast<double>(b - a);
    for (std::size_t f = req.first; f <= req.last; ++f) {
      const double t = static_cast<double>(f - a) / span;
      double v = hermite(p0, p1, m0, m1, t);
      if (v > 1.0 || v < -1.0) {
        out.overflow = true;
        v = std::clamp(v, -1.0, 1.0);
      }
      audio.sample(f, ch) = v;
    }
  } else {
    // ---- 二阶降级：取可用的三个锚点做 Lagrange 二次 ----------------
    std::vector<std::size_t> idx;
    idx.insert(idx.end(), left.begin(), left.end());
    idx.insert(idx.end(), right.begin(), right.end());
    if (idx.size() > 3) {
      // 候选最多 5 个，直接按到修复窗口中心的距离取最近三个点。
      const double center =
          static_cast<double>(req.first + req.last) / 2.0;
      std::sort(idx.begin(), idx.end(),
                [&](std::size_t x, std::size_t y) {
                  return std::fabs(static_cast<double>(x) - center) <
                         std::fabs(static_cast<double>(y) - center);
                });
      idx.resize(3);
      std::sort(idx.begin(), idx.end());
    }
    warn(out.warnings, ch, "degraded_quadratic",
         "上下文不足或单侧仍削波，降级为二阶插值");
    const double x0 = static_cast<double>(idx[0]);
    const double x1 = static_cast<double>(idx[1]);
    const double x2 = static_cast<double>(idx[2]);
    const double y0 = value(idx[0]), y1 = value(idx[1]), y2 = value(idx[2]);
    for (std::size_t f = req.first; f <= req.last; ++f) {
      const double x = static_cast<double>(f);
      double v = y0 * ((x - x1) * (x - x2)) / ((x0 - x1) * (x0 - x2)) +
                 y1 * ((x - x0) * (x - x2)) / ((x1 - x0) * (x1 - x2)) +
                 y2 * ((x - x0) * (x - x1)) / ((x2 - x0) * (x2 - x1));
      if (v > 1.0 || v < -1.0) {
        out.overflow = true;
        v = std::clamp(v, -1.0, 1.0);
      }
      audio.sample(f, ch) = v;
    }
  }

  if (out.overflow)
    warn(out.warnings, ch, "overflow_clamped",
         "插值结果超出 [-1,1]，已钳制到合法范围");
  out.ok = true;
  return out;
}

}  // namespace popfix

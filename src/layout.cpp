#include "layout.hpp"

#include <algorithm>
#include <cstdint>
#include <limits>
#include <map>
#include <string>

namespace smx {
namespace {
struct Point {
  std::size_t pos = 0;
  BreakReason reason = BreakReason::End;
};

class WidthEngine {
 public:
  WidthEngine(const std::vector<ClusterMetric>& metrics, const FontFace& font)
      : metrics_(metrics), font_(font), advance_(metrics.size() + 1, 0),
        kerning_(metrics.size() + 1, 0) {
    for (std::size_t i = 0; i < metrics.size(); ++i) {
      advance_[i + 1] = advance_[i] + metrics[i].advance26;
      if (i > 0) {
        const int k = font.kerning_between(metrics[i - 1], metrics[i]);
        kerning_[i + 1] = kerning_[i] + k;
      } else {
        kerning_[i + 1] = kerning_[i];
      }
    }
  }

  int width(std::size_t start, std::size_t end) const {
    if (start >= end || end > metrics_.size()) return 0;
    int pen = advance_[end] - advance_[start];
    pen += kerning_[end] - kerning_[start + 1];
    return pen;
  }

  int width_with_suffix(std::size_t start, std::size_t end,
                        const ClusterMetric& suffix) const {
    int pen = width(start, end);
    if (start < end && end <= metrics_.size())
      pen += font_.kerning_between(metrics_[end - 1], suffix);
    return pen + suffix.advance26;
  }

 private:
  const std::vector<ClusterMetric>& metrics_;
  const FontFace& font_;
  std::vector<int> advance_;
  std::vector<int> kerning_;
};

std::vector<Point> make_points(std::size_t seg_start, std::size_t seg_end,
                               BreakReason final_reason,
                               const std::map<std::size_t, BreakReason>& reasons) {
  std::vector<Point> points;
  points.push_back({seg_start, BreakReason::End});
  for (const auto& e : reasons) {
    if (e.first > seg_start && e.first < seg_end) points.push_back({e.first, e.second});
  }
  points.push_back({seg_end, final_reason});
  return points;
}

void append_line(std::vector<LayoutLine>& lines, std::size_t start, std::size_t end,
                 int width, BreakReason reason) {
  LayoutLine line;
  line.start = start;
  line.end = end;
  line.width26 = width;
  line.reason = reason;
  lines.push_back(line);
}

void run_greedy(const std::vector<Point>& points, const WidthEngine& widths, int max_width,
                std::vector<LayoutLine>& lines, bool& unbreakable) {
  std::size_t seg_start = points.front().pos;
  const std::size_t seg_end = points.back().pos;
  std::size_t start = seg_start;
  while (start < seg_end) {
    std::size_t chosen = start;
    BreakReason reason = BreakReason::Forced;
    for (const auto& point : points) {
      if (point.pos <= start || point.pos > seg_end) continue;
      const int w = widths.width(start, point.pos);
      if (w <= max_width) {
        chosen = point.pos;
        reason = point.reason;
      }
    }
    if (chosen == start) {
      std::size_t best = start + 1;
      for (std::size_t e = start + 1; e <= seg_end; ++e) {
        if (widths.width(start, e) <= max_width) best = e;
      }
      chosen = best;
      reason = BreakReason::Forced;
      unbreakable = true;
    }
    append_line(lines, start, chosen, widths.width(start, chosen), reason);
    start = chosen;
  }
}

void run_dp(std::vector<Point> points, const WidthEngine& widths, int max_width,
            bool is_final_segment, std::vector<LayoutLine>& lines, bool& unbreakable) {
  if (points.size() < 2) return;
  std::vector<Point> expanded;
  expanded.push_back(points.front());
  for (std::size_t k = 1; k < points.size(); ++k) {
    const std::size_t next = points[k].pos;
    std::size_t cur = expanded.back().pos;
    if (widths.width(cur, next) > max_width) {
      unbreakable = true;
      while (widths.width(cur, next) > max_width) {
        std::size_t best = cur;
        for (std::size_t e = cur + 1; e < next; ++e) {
          if (widths.width(cur, e) <= max_width) best = e;
        }
        if (best == cur) best = std::min(next, cur + 1);
        if (best >= next) break;
        expanded.push_back({best, BreakReason::Forced});
        cur = best;
      }
    }
    expanded.push_back(points[k]);
  }
  std::sort(expanded.begin(), expanded.end(),
            [](const Point& a, const Point& b) { return a.pos < b.pos; });
  expanded.erase(std::unique(expanded.begin(), expanded.end(),
                             [](const Point& a, const Point& b) { return a.pos == b.pos; }),
                 expanded.end());
  if (expanded.size() < 2) return;

  const std::size_t n = expanded.size();
  const long long inf = std::numeric_limits<long long>::max() / 8;
  std::vector<long long> cost(n, inf);
  std::vector<std::size_t> previous(n, n);
  cost[0] = 0;
  const long long max_px = static_cast<long long>(std::max(1, max_width / 64));
  for (std::size_t j = 1; j < n; ++j) {
    for (std::size_t i = 0; i < j; ++i) {
      if (cost[i] == inf) continue;
      const int w = widths.width(expanded[i].pos, expanded[j].pos);
      const bool over = w > max_width;
      if (over && j != i + 1) continue;
      const long long slack_px = static_cast<long long>(max_width - w) / 64;
      long long add = slack_px * slack_px;
      if (over) {
        const long long over_px = static_cast<long long>(w - max_width) / 64 + 1;
        add += over_px * over_px + max_px * max_px * 16;
      }
      const auto count = expanded[j].pos - expanded[i].pos;
      if (is_final_segment && j + 1 == n &&
          (count <= 1 || w < max_width / 4)) {
        add += max_px * max_px * 4;
      }
      if (cost[i] + add < cost[j]) {
        cost[j] = cost[i] + add;
        previous[j] = i;
      }
    }
  }
  if (previous[n - 1] == n) {
    run_greedy(points, widths, max_width, lines, unbreakable);
    return;
  }
  std::vector<std::size_t> path;
  for (std::size_t at = n - 1; at != 0; at = previous[at]) path.push_back(at);
  path.push_back(0);
  std::reverse(path.begin(), path.end());
  for (std::size_t k = 1; k < path.size(); ++k) {
    const std::size_t a = expanded[path[k - 1]].pos;
    const std::size_t b = expanded[path[k]].pos;
    const int w = widths.width(a, b);
    append_line(lines, a, b, w,
                w > max_width ? BreakReason::Forced : expanded[path[k]].reason);
  }
}
}  // namespace

LayoutResult layout_cue(const std::vector<Cluster>& clusters, const BreakResult& breaks,
                        FontFace& font, const LayoutOptions& options) {
  LayoutResult result;
  if (clusters.empty()) return result;
  std::vector<ClusterMetric> metrics(clusters.size());
  for (std::size_t i = 0; i < clusters.size(); ++i) {
    std::vector<std::uint32_t> missing;
    font.measure_cluster(clusters[i], metrics[i], &missing);
    for (std::uint32_t cp : missing) {
      ++result.missing_glyph_count;
      if (std::find(result.missing_glyphs.begin(), result.missing_glyphs.end(), cp) ==
          result.missing_glyphs.end())
        result.missing_glyphs.push_back(cp);
    }
  }
  result.missing_glyph = !result.missing_glyphs.empty();

  std::map<std::size_t, BreakReason> reason_at;
  for (const auto& b : breaks.breaks) reason_at[b.pos] = b.reason;
  std::vector<bool> hard(clusters.size(), false);
  for (std::size_t i = 0; i < clusters.size() && i < breaks.hard_after.size(); ++i)
    hard[i] = breaks.hard_after[i] || clusters[i].first_cp == '\n' ||
              clusters[i].first_cp == '\r';

  const WidthEngine widths(metrics, font);
  std::vector<LayoutLine> lines;
  std::size_t pos = 0;
  while (pos < clusters.size()) {
    if (hard[pos]) {
      append_line(lines, pos, pos, 0, BreakReason::Hard);
      ++pos;
      continue;
    }
    std::size_t seg_end = pos;
    while (seg_end < clusters.size() && !hard[seg_end]) ++seg_end;
    const BreakReason final_reason =
        seg_end < clusters.size() ? BreakReason::Hard : BreakReason::End;
    const auto points = make_points(pos, seg_end, final_reason, reason_at);
    if (options.strategy == LayoutStrategy::Greedy)
      run_greedy(points, widths, options.max_width26, lines, result.unbreakable);
    else
      run_dp(points, widths, options.max_width26, seg_end == clusters.size(), lines,
             result.unbreakable);
    if (seg_end < clusters.size()) ++seg_end;
    pos = seg_end;
  }

  result.natural_line_count = lines.size();
  result.lines_overflow = options.max_lines > 0 &&
                          static_cast<int>(lines.size()) > options.max_lines;
  ClusterMetric ellipsis_metric;
  Cluster ellipsis_cluster;
  ellipsis_cluster.cps.push_back(0x2026);
  ellipsis_cluster.first_cp = 0x2026;
  font.measure_cluster(ellipsis_cluster, ellipsis_metric, nullptr);
  const bool have_ellipsis = true;

  if (result.lines_overflow && options.truncate == TruncatePolicy::Ellipsis &&
      options.max_lines > 0 && have_ellipsis) {
    const std::size_t keep = static_cast<std::size_t>(options.max_lines - 1);
    if (keep < lines.size()) {
      const std::size_t start = lines[keep].start;
      std::size_t limit = start;
      while (limit < clusters.size() && !hard[limit]) ++limit;
      std::size_t best = start;
      for (std::size_t e = start; e <= limit; ++e) {
        if (widths.width_with_suffix(start, e, ellipsis_metric) <= options.max_width26)
          best = e;
      }
      LayoutLine truncated;
      truncated.start = start;
      truncated.end = best;
      truncated.width26 = widths.width_with_suffix(start, best, ellipsis_metric);
      truncated.reason = BreakReason::Ellipsis;
      truncated.ellipsis = true;
      lines.resize(keep);
      lines.push_back(truncated);
    }
  }

  result.lines = std::move(lines);
  for (std::size_t i = 0; i < result.lines.size(); ++i) {
    auto& line = result.lines[i];
    line.bbox = measure_line_bbox(metrics, line.start, line.end,
                                  line.ellipsis ? &ellipsis_metric : nullptr, font,
                                  static_cast<int>(i), font.ascender26(),
                                  font.line_height26());
    if (line.width26 > options.max_width26) result.width_overflow = true;
  }
  for (const auto& line : result.lines) add_bbox(result.bbox, line.bbox);
  return result;
}
}  // namespace smx

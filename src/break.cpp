#include "types.h"

#include <algorithm>
#include <limits>

namespace sublay {

// Penalty table (deterministic; same metrics + width => same breaks).
static const long kLineBase = 2;        // prefer fewer lines
static const long kOverflowBase = 1000; // any over-wide line
static const long kOverflowPerPx = 100;
static const long kOrphan = 50;         // single-cluster last line
static const long kKinsoku = 40;        // forbidden line start/end char
static const long kWordCut = 200;       // word-internal hard cut

// Lay out clusters [a, b) of one forced-break segment with at most cap lines.
// Fills res lines/breaks; returns false only when b == a (empty segment).
static void layout_segment(const std::vector<ClusterInfo>& cl, size_t a, size_t b,
                           int cap, fixed_t safe_width, size_t text_end,
                           BreakResult& res) {
  if (a == b) return;
  // candidate break positions inside (a, b)
  std::vector<size_t> pos;
  pos.push_back(a);
  for (size_t k = a + 1; k < b; ++k)
    if (cl[k - 1].break_after) pos.push_back(k);
  pos.push_back(b);
  const size_t P = pos.size();
  const long INF = std::numeric_limits<long>::max() / 4;

  // width of clusters [i, j) with trailing collapsible spaces removed
  auto line_width = [&](size_t i, size_t j) {
    fixed_t w = 0;
    for (size_t k = i; k < j; ++k) w += cl[k].width;
    size_t k = j;
    while (k > i && cl[k - 1].trailing_space) {
      w -= cl[k - 1].width;
      --k;
    }
    return w;
  };
  auto line_cost = [&](size_t pi, size_t pj, bool last_seg_line) {
    size_t i = pos[pi], j = pos[pj];
    long cost = kLineBase;
    fixed_t w = line_width(i, j);
    if (w > safe_width)
      cost += kOverflowBase + kOverflowPerPx * ((w - safe_width) >> 6);
    if (last_seg_line && j - i == 1 && b - a > 1) cost += kOrphan;
    if (j < b) {  // the break after this line
      if (cl[j].no_line_start) cost += kKinsoku;
      if (cl[j - 1].no_line_end) cost += kKinsoku;
      if (cl[j - 1].word_cut) cost += kWordCut;
    }
    return cost;
  };

  int L = std::max(1, cap);
  std::vector<std::vector<long>> dp(P, std::vector<long>(L + 1, INF));
  std::vector<std::vector<int>> par(P, std::vector<int>(L + 1, -1));
  dp[0][0] = 0;
  for (size_t pj = 1; pj < P; ++pj)
    for (size_t pi = 0; pi < pj; ++pi)
      for (int l = 0; l < L; ++l) {
        if (dp[pi][l] >= INF) continue;
        bool last = pj == P - 1;
        long c = dp[pi][l] + line_cost(pi, pj, last);
        if (c < dp[pj][l + 1]) {
          dp[pj][l + 1] = c;
          par[pj][l + 1] = static_cast<int>(pi);
        }
      }
  int best_l = 1;
  for (int l = 2; l <= L; ++l)
    if (dp[P - 1][l] < dp[P - 1][best_l]) best_l = l;
  if (dp[P - 1][best_l] >= INF) return;  // unreachable, defensive

  // reconstruct
  std::vector<size_t> bounds;  // cluster indices of line starts, plus b
  size_t pj = P - 1;
  int l = best_l;
  bounds.push_back(b);
  while (pj > 0) {
    int pi = par[pj][l];
    if (pi < 0) break;
    bounds.push_back(pos[pi]);
    pj = static_cast<size_t>(pi);
    --l;
  }
  std::reverse(bounds.begin(), bounds.end());
  for (size_t t = 0; t + 1 < bounds.size(); ++t) {
    size_t i = bounds[t], j = bounds[t + 1];
    size_t start_byte = cl[i].byte_off;
    size_t end_byte = j < cl.size() ? cl[j].byte_off : text_end;
    res.line_start.push_back(start_byte);
    res.line_end.push_back(end_byte);
    res.line_widths.push_back(line_width(i, j));
    if (j < b) res.breaks.push_back(cl[j].byte_off);  // in-segment break
  }
}

BreakResult layout_lines(const std::vector<ClusterInfo>& clusters,
                         const std::vector<size_t>& forced_breaks,
                         fixed_t safe_width, int max_lines) {
  BreakResult res;
  const size_t N = clusters.size();
  if (N == 0) return res;
  size_t text_end = clusters[N - 1].byte_off + clusters[N - 1].byte_len;

  // forced byte offsets -> cluster indices (sorted, duplicates kept)
  std::vector<size_t> forced;
  for (size_t o : forced_breaks) {
    size_t k = 0;
    while (k < N && clusters[k].byte_off < o) ++k;
    forced.push_back(k);
  }
  std::sort(forced.begin(), forced.end());

  // segments between forced breaks
  std::vector<std::pair<size_t, size_t>> segs;
  size_t prev = 0;
  for (size_t k : forced) {
    segs.emplace_back(prev, k);
    prev = k;
  }
  segs.emplace_back(prev, N);

  int lines_used = 0;
  for (size_t s = 0; s < segs.size(); ++s) {
    size_t a = segs[s].first, b = segs[s].second;
    if (a == b) {  // empty line from consecutive/leading/trailing forced break
      size_t at = a < N ? clusters[a].byte_off : text_end;
      res.line_start.push_back(at);
      res.line_end.push_back(at);
      res.line_widths.push_back(0);
      ++lines_used;
    } else {
      int remaining = max_lines - lines_used - static_cast<int>(segs.size() - s - 1);
      int cap = std::max(1, remaining);
      size_t before = res.line_start.size();
      layout_segment(clusters, a, b, cap, safe_width, text_end, res);
      lines_used += static_cast<int>(res.line_start.size() - before);
    }
  }

  // forced breaks must appear in the breaks list (byte offsets, ascending;
  // consecutive forced breaks share an offset and stay as duplicates)
  for (size_t k : forced) {
    size_t at = k < N ? clusters[k].byte_off : text_end;
    res.breaks.push_back(at);
  }
  std::sort(res.breaks.begin(), res.breaks.end());

  // overflow evaluation
  fixed_t max_excess = 0;
  bool wide = false;
  for (fixed_t w : res.line_widths) {
    if (w > safe_width) {
      wide = true;
      max_excess = std::max(max_excess, w - safe_width);
    }
  }
  bool too_many = static_cast<int>(res.line_start.size()) > max_lines;
  res.overflow = wide || too_many;
  res.overflow_px = max_excess;
  if (wide) res.rules.push_back("width");
  if (too_many) res.rules.push_back("max_lines");
  return res;
}

}  // namespace sublay

#include "types.h"

#include <algorithm>
#include <thread>

namespace sublay {

static bool is_word_cp(uint32_t cp) {
  if (is_space_cp(cp) || is_cjk_cp(cp) || is_rtl_cp(cp)) return false;
  if (is_combining(cp) || is_variation_selector(cp) || is_zwj(cp)) return false;
  if (kinsoku_no_line_start(cp) || kinsoku_no_line_end(cp)) return false;
  if (cp < 0x80 && !((cp >= 'a' && cp <= 'z') || (cp >= 'A' && cp <= 'Z') ||
                     (cp >= '0' && cp <= '9') || cp == '-' || cp == '\''))
    return false;
  return true;
}

// Mark clusters covered by http(s)://... spans as unbreakable URLs.
static std::vector<bool> find_url_spans(const std::string& text,
                                        const std::vector<Cluster>& clusters) {
  std::vector<bool> url(clusters.size(), false);
  auto mark_from = [&](size_t pos) {
    size_t end = pos;
    while (end < text.size() && is_url_char(static_cast<unsigned char>(text[end])))
      ++end;
    for (size_t i = 0; i < clusters.size(); ++i)
      if (clusters[i].byte_off < end &&
          clusters[i].byte_off + clusters[i].byte_len > pos)
        url[i] = true;
  };
  size_t p = 0;
  while ((p = text.find("http", p)) != std::string::npos) {
    bool https = text.compare(p, 8, "https://") == 0;
    bool http = text.compare(p, 7, "http://") == 0;
    if (https || http) mark_from(p);
    p += 4;
  }
  return url;
}

CueMetrics layout_cue(const Cue& cue, FontChain& fonts, const LayoutParams& params) {
  CueMetrics m;
  m.cue_id = cue.id;
  StyledText st;
  std::string err;
  if (!strip_markup(cue.text, st, err)) return m;  // parser already validated
  std::vector<UChar> cps;
  size_t bad = 0;
  if (!utf8_decode(st.text, cps, bad)) return m;
  std::vector<Cluster> clusters = make_clusters(cps, st.text.size());
  if (clusters.empty()) return m;  // empty / whitespace-only cue

  const size_t N = clusters.size();
  std::vector<bool> url = find_url_spans(st.text, clusters);

  // per-cluster style
  auto style_at = [&](size_t off) {
    Style s;
    auto test = [&](const std::vector<std::pair<size_t, size_t>>& spans, bool& f) {
      for (auto& sp : spans)
        if (off >= sp.first && off < sp.second) f = true;
    };
    test(st.bold, s.bold);
    test(st.italic, s.italic);
    test(st.underline, s.underline);
    return s;
  };

  // font selection + run building (uniform font/direction/style per run)
  std::vector<int> font_of(N, -1);
  std::vector<bool> missing(N, false);
  for (size_t i = 0; i < N; ++i) {
    if (clusters[i].zero_width) continue;
    int fi = fonts.pick_font(clusters[i].first_cp);
    if (fi < 0) {
      missing[i] = true;
      m.missing_glyph = true;
    } else {
      font_of[i] = fi;
    }
  }
  struct RunDef {
    size_t begin, end;  // cluster range
    int font;
    bool rtl;
    Style style;
  };
  std::vector<RunDef> runs;
  for (size_t i = 0; i < N;) {
    if (clusters[i].zero_width || missing[i]) { ++i; continue; }
    RunDef r{i, i + 1, font_of[i], clusters[i].rtl, style_at(clusters[i].byte_off)};
    size_t j = i + 1;
    while (j < N && !clusters[j].zero_width && !missing[j] &&
           font_of[j] == r.font && clusters[j].rtl == r.rtl &&
           style_at(clusters[j].byte_off) == r.style)
      ++j;
    r.end = j;
    runs.push_back(r);
    i = j;
  }
  // visual order: reverse contiguous RTL groups (simplified bidi)
  std::vector<size_t> vis(runs.size());
  for (size_t i = 0; i < runs.size(); ++i) vis[i] = i;
  for (size_t i = 0; i < runs.size();) {
    if (!runs[vis[i]].rtl) { ++i; continue; }
    size_t j = i;
    while (j < runs.size() && runs[vis[j]].rtl) ++j;
    std::reverse(vis.begin() + i, vis.begin() + j);
    i = j;
  }

  // shape runs in visual order, accumulate per-cluster widths and ink boxes
  std::vector<fixed_t> cw(N, 0);
  std::vector<fixed_t> top(N, 0), bot(N, 0);
  std::vector<bool> has_ink(N, false);
  std::vector<size_t> coffs;
  for (auto& c : clusters) coffs.push_back(c.byte_off);
  auto cluster_index = [&](size_t off) -> size_t {
    size_t lo = 0, hi = N;
    while (lo < hi) {
      size_t mid = (lo + hi) / 2;
      if (coffs[mid] < off) lo = mid + 1; else hi = mid;
    }
    if (lo < N && coffs[lo] == off) return lo;
    return lo > 0 ? lo - 1 : 0;
  };
  for (size_t vi : vis) {
    const RunDef& r = runs[vi];
    TextRun tr;
    tr.byte_off = clusters[r.begin].byte_off;
    size_t end_off = clusters[r.end - 1].byte_off + clusters[r.end - 1].byte_len;
    tr.text = st.text.substr(tr.byte_off, end_off - tr.byte_off);
    tr.lang = cue.lang;
    tr.font_index = r.font;
    tr.style = r.style;
    tr.rtl = r.rtl;
    std::vector<GlyphBox> glyphs;
    fonts.shape_run(tr, glyphs);
    m.glyph_count += static_cast<int>(glyphs.size());
    for (auto& g : glyphs) {
      size_t ci = cluster_index(g.cluster);
      cw[ci] += g.advance;
      fixed_t t = g.bearing_y, b = g.bearing_y - g.height;
      if (!has_ink[ci]) {
        top[ci] = t; bot[ci] = b; has_ink[ci] = true;
      } else {
        top[ci] = std::max(top[ci], t);
        bot[ci] = std::min(bot[ci], b);
      }
    }
  }
  for (size_t i = 0; i < N; ++i) {
    if (clusters[i].zero_width) cw[i] = 0;
    if (missing[i]) cw[i] = fonts.missing_advance();
  }

  // break candidates
  std::vector<ClusterInfo> cis(N);
  for (size_t i = 0; i < N; ++i) {
    cis[i].byte_off = clusters[i].byte_off;
    cis[i].byte_len = clusters[i].byte_len;
    cis[i].width = cw[i];
    cis[i].no_line_start = kinsoku_no_line_start(clusters[i].first_cp);
    cis[i].no_line_end = kinsoku_no_line_end(clusters[i].first_cp);
    cis[i].trailing_space = is_space_cp(clusters[i].first_cp);
    if (url[i]) continue;  // never break inside a URL
    uint32_t cp = clusters[i].first_cp;
    bool next_cjk = i + 1 < N && is_cjk_cp(clusters[i + 1].first_cp);
    if (is_space_cp(cp) || is_cjk_cp(cp) || next_cjk || cp == 0x2D) {
      cis[i].break_after = true;
    } else if (is_word_cp(cp) && i + 1 < N && is_word_cp(clusters[i + 1].first_cp) &&
               !url[i + 1]) {
      cis[i].break_after = true;  // last-resort word-internal cut
      cis[i].word_cut = true;
    }
  }

  fixed_t safe_width = ft_from_int(params.width - 2 * params.safe);
  if (safe_width < 0) safe_width = 0;
  BreakResult br = layout_lines(cis, st.forced_breaks, safe_width, params.max_lines);

  // line boxes with baselines; visual-order x accumulation
  m.breaks = br.breaks;
  m.line_count = static_cast<int>(br.line_start.size());
  m.overflow = br.overflow;
  m.overflow_px = br.overflow_px;
  m.rules = br.rules;
  fixed_t lh = fonts.line_height();
  fixed_t asc = fonts.ascender();
  // visual cluster order per line
  std::vector<int> vis_cluster(N);
  for (size_t i = 0; i < N; ++i) vis_cluster[i] = static_cast<int>(i);
  {
    std::vector<size_t> order;
    for (size_t vi : vis) {
      const RunDef& r = runs[vi];
      if (r.rtl)
        for (size_t k = r.end; k-- > r.begin;) order.push_back(k);
      else
        for (size_t k = r.begin; k < r.end; ++k) order.push_back(k);
    }
    std::vector<char> seen(N, 0);
    size_t w = 0;
    for (size_t k : order)
      if (!seen[k]) { seen[k] = 1; vis_cluster[w++] = static_cast<int>(k); }
    for (size_t k = 0; k < N; ++k)
      if (!seen[k]) vis_cluster[w++] = static_cast<int>(k);
  }
  bool first_line = true;
  for (int li = 0; li < m.line_count; ++li) {
    LineBox lb;
    lb.start_byte = br.line_start[li];
    lb.end_byte = br.line_end[li];
    lb.width = br.line_widths[li];
    lb.baseline = asc + lh * li;
    fixed_t x = 0;
    bool any = false;
    fixed_t x0 = 0, x1 = 0, y0 = 0, y1 = 0;
    for (size_t k = 0; k < N; ++k) {
      size_t ci = static_cast<size_t>(vis_cluster[k]);
      if (clusters[ci].byte_off < lb.start_byte || clusters[ci].byte_off >= lb.end_byte)
        continue;
      fixed_t adv = cw[ci];
      if (has_ink[ci]) {
        fixed_t gx0 = x, gx1 = x + adv;
        fixed_t gy0 = lb.baseline - top[ci], gy1 = lb.baseline - bot[ci];
        if (!any) { x0 = gx0; x1 = gx1; y0 = gy0; y1 = gy1; any = true; }
        else {
          x0 = std::min(x0, gx0); x1 = std::max(x1, gx1);
          y0 = std::min(y0, gy0); y1 = std::max(y1, gy1);
        }
      }
      x += adv;
    }
    if (!any) {  // empty line: line-height box at origin
      x0 = 0; x1 = 0;
      y0 = lb.baseline - asc;
      y1 = lb.baseline - (asc - lh);
    }
    lb.x_min = x0; lb.x_max = x1; lb.y_min = y0; lb.y_max = y1;
    m.lines.push_back(lb);
    if (first_line) {
      m.bbox = lb;
      first_line = false;
    } else {
      m.bbox.x_min = std::min(m.bbox.x_min, lb.x_min);
      m.bbox.y_min = std::min(m.bbox.y_min, lb.y_min);
      m.bbox.x_max = std::max(m.bbox.x_max, lb.x_max);
      m.bbox.y_max = std::max(m.bbox.y_max, lb.y_max);
    }
  }
  m.bbox.baseline = m.lines.empty() ? 0 : m.lines[0].baseline;
  return m;
}

MeasureOutput measure_all(const std::vector<Cue>& cues, FontChain& fonts,
                          const LayoutParams& params, int jobs) {
  MeasureOutput out;
  const size_t n = cues.size();
  out.cues.resize(n);
  int nj = jobs < 1 ? 1 : jobs;
  size_t shards = std::min<size_t>(n, static_cast<size_t>(nj));
  std::vector<std::thread> pool;
  for (size_t s = 0; s < shards; ++s) {
    pool.emplace_back([&, s] {
      for (size_t i = s; i < n; i += shards)
        out.cues[i] = layout_cue(cues[i], fonts, params);
    });
  }
  for (auto& t : pool) t.join();
  for (auto& m : out.cues) {
    out.stats.cues += 1;
    out.stats.glyphs += m.glyph_count;
    out.stats.breaks += static_cast<long>(m.breaks.size());
    if (m.overflow) out.stats.overflow_count += 1;
    if (m.missing_glyph) out.stats.missing_glyph += 1;
  }
  return out;
}

}  // namespace sublay

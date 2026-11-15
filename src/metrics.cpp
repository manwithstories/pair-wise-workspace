#include "metrics.hpp"

#include <algorithm>

namespace smx {
BBox glyph_bbox(const GlyphMetric& glyph, int pen_x26, int baseline_y26) {
  BBox box;
  if (glyph.width26 <= 0 || glyph.height26 <= 0) return box;
  const int left = pen_x26 + glyph.x_offset26 + glyph.bearing_x26;
  const int top = baseline_y26 - glyph.bearing_y26;
  box.x26 = left;
  box.y26 = top;
  box.w26 = glyph.width26;
  box.h26 = glyph.height26;
  box.valid = true;
  return box;
}

void add_bbox(BBox& dst, const BBox& src) {
  if (!src.valid) return;
  if (!dst.valid) {
    dst = src;
    return;
  }
  const int left = std::min(dst.x26, src.x26);
  const int top = std::min(dst.y26, src.y26);
  const int right = std::max(dst.x26 + dst.w26, src.x26 + src.w26);
  const int bottom = std::max(dst.y26 + dst.h26, src.y26 + src.h26);
  dst.x26 = left;
  dst.y26 = top;
  dst.w26 = right - left;
  dst.h26 = bottom - top;
  dst.valid = true;
}

BBox measure_line_bbox(const std::vector<ClusterMetric>& metrics, std::size_t start,
                       std::size_t end, const ClusterMetric* suffix,
                       const FontFace& font, int line_index, int ascender26,
                       int line_height26) {
  BBox box;
  if (start >= metrics.size() || end > metrics.size() || start >= end) return box;
  const int baseline = line_index * line_height26 + ascender26;
  int pen = 0;
  for (std::size_t i = start; i < end; ++i) {
    if (i > start) pen += font.kerning_between(metrics[i - 1], metrics[i]);
    for (const auto& glyph : metrics[i].glyphs)
      add_bbox(box, glyph_bbox(glyph, pen, baseline));
    pen += metrics[i].advance26;
  }
  if (suffix) {
    if (end > start) pen += font.kerning_between(metrics[end - 1], *suffix);
    for (const auto& glyph : suffix->glyphs)
      add_bbox(box, glyph_bbox(glyph, pen, baseline));
  }
  return box;
}

BBox union_bbox(const std::vector<BBox>& boxes) {
  BBox out;
  for (const auto& box : boxes) add_bbox(out, box);
  return out;
}
}  // namespace smx

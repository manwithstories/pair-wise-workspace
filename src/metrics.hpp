#pragma once
#include <cstddef>
#include <vector>
#include "font_face.hpp"

namespace smx {
struct BBox {
  int x26 = 0;
  int y26 = 0;
  int w26 = 0;
  int h26 = 0;
  bool valid = false;
};

BBox glyph_bbox(const GlyphMetric& glyph, int pen_x26, int baseline_y26);
void add_bbox(BBox& dst, const BBox& src);
BBox measure_line_bbox(const std::vector<ClusterMetric>& metrics, std::size_t start,
                       std::size_t end, const ClusterMetric* suffix,
                       const FontFace& font, int line_index, int ascender26,
                       int line_height26);
BBox union_bbox(const std::vector<BBox>& boxes);
}  // namespace smx

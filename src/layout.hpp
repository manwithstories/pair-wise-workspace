#pragma once
#include <cstddef>
#include <cstdint>
#include <vector>
#include "break_opp.hpp"
#include "font_face.hpp"
#include "metrics.hpp"
#include "utf8.hpp"

namespace smx {
enum class LayoutStrategy { Greedy, Dp };
enum class TruncatePolicy { Keep, Ellipsis };

struct LayoutOptions {
  int max_width26 = 0;
  int max_lines = 0;
  LayoutStrategy strategy = LayoutStrategy::Dp;
  TruncatePolicy truncate = TruncatePolicy::Keep;
};

struct LayoutLine {
  std::size_t start = 0;
  std::size_t end = 0;
  int width26 = 0;
  BBox bbox;
  BreakReason reason = BreakReason::End;
  bool ellipsis = false;
};

struct LayoutResult {
  std::vector<LayoutLine> lines;
  BBox bbox;
  std::size_t natural_line_count = 0;
  std::vector<std::uint32_t> missing_glyphs;
  std::size_t missing_glyph_count = 0;
  bool width_overflow = false;
  bool lines_overflow = false;
  bool unbreakable = false;
  bool missing_glyph = false;

  bool ok() const {
    return !width_overflow && !lines_overflow && !unbreakable && !missing_glyph;
  }
  bool has_overflow() const { return !ok(); }
};

LayoutResult layout_cue(const std::vector<Cluster>& clusters, const BreakResult& breaks,
                        FontFace& font, const LayoutOptions& options);
}  // namespace smx

#pragma once
#include <cstdint>
#include <string>
#include <unordered_map>
#include <vector>
#include <ft2build.h>
#include FT_FREETYPE_H
#include "utf8.hpp"

namespace smx {
struct GlyphMetric {
  std::uint32_t cp = 0;
  int face_index = 0;
  int glyph_index = 0;
  int advance26 = 0;
  int bearing_x26 = 0;
  int bearing_y26 = 0;
  int width26 = 0;
  int height26 = 0;
  int x_offset26 = 0;
  bool missing = false;
};

struct ClusterMetric {
  std::vector<GlyphMetric> glyphs;
  int advance26 = 0;
};

class FontFace {
 public:
  FontFace();
  ~FontFace();
  FontFace(const FontFace&) = delete;
  FontFace& operator=(const FontFace&) = delete;

  bool open(const std::string& primary, const std::vector<std::string>& fallbacks,
            int pixel_size, std::string& error);
  bool measure_cluster(const Cluster& cluster, ClusterMetric& metric,
                       std::vector<std::uint32_t>* missing);
  int kerning_between(const ClusterMetric& previous, const ClusterMetric& current) const;
  int line_height26() const { return line_height26_; }
  int ascender26() const { return ascender26_; }
  int descender26() const { return descender26_; }
  int pixel_size() const { return pixel_size_; }
  const std::string& error() const { return error_; }

 private:
  struct FaceEntry {
    FT_Face face = nullptr;
    std::string path;
  };
  struct CachedGlyph {
    GlyphMetric metric;
  };
  bool load_glyph(std::uint32_t cp, GlyphMetric& glyph);
  void release();
  void set_error(const std::string& what, FT_Error err);

  FT_Library library_ = nullptr;
  std::vector<FaceEntry> faces_;
  std::unordered_map<std::uint32_t, CachedGlyph> glyph_cache_;
  int pixel_size_ = 0;
  int ascender26_ = 0;
  int descender26_ = 0;
  int line_height26_ = 0;
  std::string error_;
};
}  // namespace smx

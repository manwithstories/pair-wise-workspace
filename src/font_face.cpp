#include "font_face.hpp"

#include <algorithm>
#include <limits>

namespace smx {
namespace {
bool is_invisible_control(std::uint32_t cp) {
  return cp == '\n' || cp == '\r' || cp == '\t' || cp == 0x200b ||
         cp == 0x200c || cp == 0x200d || cp == 0x2060 || cp == 0xfeff ||
         (cp >= 0xfe00 && cp <= 0xfe0f);
}
}  // namespace

FontFace::FontFace() = default;

FontFace::~FontFace() { release(); }

void FontFace::release() {
  for (auto& f : faces_) {
    if (f.face) FT_Done_Face(f.face);
    f.face = nullptr;
  }
  faces_.clear();
  glyph_cache_.clear();
  if (library_) {
    FT_Done_FreeType(library_);
    library_ = nullptr;
  }
}

void FontFace::set_error(const std::string& what, FT_Error err) {
  error_ = what + " (FreeType error " + std::to_string(err) + ")";
}

bool FontFace::open(const std::string& primary, const std::vector<std::string>& fallbacks,
                    int pixel_size, std::string& error) {
  release();
  error_.clear();
  pixel_size_ = pixel_size;
  if (pixel_size <= 0) {
    error_ = "font size must be positive";
    error = error_;
    return false;
  }
  FT_Error err = FT_Init_FreeType(&library_);
  if (err) {
    set_error("cannot initialize FreeType", err);
    error = error_;
    return false;
  }
  std::vector<std::string> paths;
  paths.push_back(primary);
  paths.insert(paths.end(), fallbacks.begin(), fallbacks.end());
  for (const auto& path : paths) {
    if (path.empty()) continue;
    FT_Face face = nullptr;
    err = FT_New_Face(library_, path.c_str(), 0, &face);
    if (err) {
      error_ = "cannot open font: " + path + " (FreeType error " + std::to_string(err) + ")";
      release();
      error = error_;
      return false;
    }
    err = FT_Set_Pixel_Sizes(face, 0, static_cast<FT_UInt>(pixel_size));
    if (err) {
      set_error("cannot set font size for " + path, err);
      FT_Done_Face(face);
      release();
      error = error_;
      return false;
    }
    (void)FT_Select_Charmap(face, FT_ENCODING_UNICODE);
    faces_.push_back({face, path});
  }
  if (faces_.empty()) {
    error_ = "no font face loaded";
    release();
    error = error_;
    return false;
  }
  const auto& m = faces_.front().face->size->metrics;
  ascender26_ = static_cast<int>(m.ascender);
  descender26_ = static_cast<int>(m.descender);
  line_height26_ = static_cast<int>(m.height);
  if (line_height26_ < ascender26_ - descender26_)
    line_height26_ = ascender26_ - descender26_;
  return true;
}

bool FontFace::load_glyph(std::uint32_t cp, GlyphMetric& glyph) {
  glyph = GlyphMetric{};
  glyph.cp = cp;
  if (is_invisible_control(cp)) return true;
  const std::uint32_t lookup_cp = cp;
  std::uint32_t actual_cp = lookup_cp;
  for (std::size_t fi = 0; fi < faces_.size(); ++fi) {
    FT_Face face = faces_[fi].face;
    FT_UInt gid = FT_Get_Char_Index(face, actual_cp);
    if (gid == 0) continue;
    FT_Error err = FT_Load_Glyph(face, gid, FT_LOAD_NO_HINTING | FT_LOAD_NO_BITMAP);
    if (err) continue;
    FT_GlyphSlot slot = face->glyph;
    glyph.face_index = static_cast<int>(fi);
    glyph.glyph_index = static_cast<int>(gid);
    glyph.advance26 = static_cast<int>(slot->advance.x);
    glyph.bearing_x26 = static_cast<int>(slot->metrics.horiBearingX);
    glyph.bearing_y26 = static_cast<int>(slot->metrics.horiBearingY);
    glyph.width26 = static_cast<int>(slot->metrics.width);
    glyph.height26 = static_cast<int>(slot->metrics.height);
    glyph.missing = false;
    return true;
  }
  glyph.missing = true;
  if (cp != 0xfffd) {
    for (std::size_t fi = 0; fi < faces_.size(); ++fi) {
      FT_Face face = faces_[fi].face;
      const FT_UInt gid = FT_Get_Char_Index(face, 0xfffd);
      if (gid == 0) continue;
      if (FT_Load_Glyph(face, gid, FT_LOAD_NO_HINTING | FT_LOAD_NO_BITMAP) == 0) {
        FT_GlyphSlot slot = face->glyph;
        glyph.face_index = static_cast<int>(fi);
        glyph.glyph_index = static_cast<int>(gid);
        glyph.advance26 = static_cast<int>(slot->advance.x);
        glyph.bearing_x26 = static_cast<int>(slot->metrics.horiBearingX);
        glyph.bearing_y26 = static_cast<int>(slot->metrics.horiBearingY);
        glyph.width26 = static_cast<int>(slot->metrics.width);
        glyph.height26 = static_cast<int>(slot->metrics.height);
        return true;
      }
    }
  }
  glyph.face_index = 0;
  glyph.glyph_index = 0;
  if (!faces_.empty()) {
    FT_Face face = faces_.front().face;
    if (FT_Load_Glyph(face, 0, FT_LOAD_NO_HINTING | FT_LOAD_NO_BITMAP) == 0) {
      FT_GlyphSlot slot = face->glyph;
      glyph.advance26 = static_cast<int>(slot->advance.x);
      glyph.bearing_x26 = static_cast<int>(slot->metrics.horiBearingX);
      glyph.bearing_y26 = static_cast<int>(slot->metrics.horiBearingY);
      glyph.width26 = static_cast<int>(slot->metrics.width);
      glyph.height26 = static_cast<int>(slot->metrics.height);
    }
  }
  return true;
}

bool FontFace::measure_cluster(const Cluster& cluster, ClusterMetric& metric,
                               std::vector<std::uint32_t>* missing) {
  metric = ClusterMetric{};
  int pen = 0;
  for (std::uint32_t cp : cluster.cps) {
    auto it = glyph_cache_.find(cp);
    if (it == glyph_cache_.end()) {
      CachedGlyph cached;
      load_glyph(cp, cached.metric);
      it = glyph_cache_.emplace(cp, cached).first;
    }
    GlyphMetric glyph = it->second.metric;
    glyph.x_offset26 = pen;
    if (glyph.missing && missing) missing->push_back(cp);
    metric.glyphs.push_back(glyph);
    pen += glyph.advance26;
  }
  metric.advance26 = pen;
  return true;
}

int FontFace::kerning_between(const ClusterMetric& previous,
                              const ClusterMetric& current) const {
  if (previous.glyphs.empty() || current.glyphs.empty()) return 0;
  const GlyphMetric& left = previous.glyphs.back();
  const GlyphMetric& right = current.glyphs.front();
  if (left.missing || right.missing || left.face_index != right.face_index) return 0;
  if (left.face_index < 0 || static_cast<std::size_t>(left.face_index) >= faces_.size())
    return 0;
  FT_Vector delta{};
  FT_Face face = faces_[static_cast<std::size_t>(left.face_index)].face;
  const FT_Error err = FT_Get_Kerning(face, static_cast<FT_UInt>(left.glyph_index),
                                      static_cast<FT_UInt>(right.glyph_index),
                                      FT_KERNING_UNFITTED, &delta);
  if (err) return 0;
  return static_cast<int>(delta.x);
}
}  // namespace smx

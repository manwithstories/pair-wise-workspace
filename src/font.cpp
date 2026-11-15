#include "types.h"

#include <ft2build.h>
#include FT_FREETYPE_H
#include <hb.h>
#include <hb-ft.h>

#include <mutex>
#include <string>
#include <vector>

namespace sublay {

struct FontChain::Impl {
  FT_Library lib = nullptr;
  std::vector<FT_Face> faces;
  std::vector<hb_font_t*> hb_fonts;
  std::vector<hb_buffer_t*> buffers;  // one scratch buffer per font slot
  int px = 0;
  fixed_t asc = 0, desc = 0, line_h = 0, missing_adv = 0;
  std::mutex mu;
};

FontChain::FontChain() : p_(new Impl) {}

FontChain::~FontChain() {
  if (!p_) return;
  for (auto* b : p_->buffers)
    if (b) hb_buffer_destroy(b);
  for (auto* f : p_->hb_fonts)
    if (f) hb_font_destroy(f);
  for (auto f : p_->faces)
    if (f) FT_Done_Face(f);
  if (p_->lib) FT_Done_FreeType(p_->lib);
  delete p_;
}

bool FontChain::load(const std::vector<std::string>& paths, int size_px,
                     std::string& err) {
  if (size_px <= 0) {
    err = "font size must be > 0";
    return false;
  }
  if (paths.empty()) {
    err = "no font given";
    return false;
  }
  if (FT_Init_FreeType(&p_->lib) != 0) {
    err = "freetype init failed";
    return false;
  }
  p_->px = size_px;
  for (const auto& path : paths) {
    FT_Face face = nullptr;
    FT_Error e = FT_New_Face(p_->lib, path.c_str(), 0, &face);
    if (e != 0 || !face) {
      err = "font unusable: " + path;
      return false;
    }
    if (FT_Set_Pixel_Sizes(face, 0, static_cast<FT_UInt>(size_px)) != 0) {
      FT_Done_Face(face);
      err = "cannot set size on: " + path;
      return false;
    }
    p_->faces.push_back(face);
    hb_font_t* hf = hb_ft_font_create(face, nullptr);
    hb_font_set_scale(hf, size_px << 6, size_px << 6);
    p_->hb_fonts.push_back(hf);
    p_->buffers.push_back(hb_buffer_create());
  }
  FT_Size_Metrics m = p_->faces[0]->size->metrics;
  p_->asc = static_cast<fixed_t>(m.ascender);
  p_->desc = static_cast<fixed_t>(m.descender);
  p_->line_h = static_cast<fixed_t>(m.height);
  if (p_->line_h <= 0) p_->line_h = p_->asc - p_->desc;
  p_->missing_adv = static_cast<fixed_t>(size_px << 5);  // half em
  return true;
}

int FontChain::pick_font(uint32_t cp) const {
  std::lock_guard<std::mutex> lk(p_->mu);
  for (size_t i = 0; i < p_->faces.size(); ++i) {
    if (FT_Get_Char_Index(p_->faces[i], cp) != 0) return static_cast<int>(i);
  }
  return -1;
}

void FontChain::shape_run(const TextRun& run, std::vector<GlyphBox>& out) {
  std::lock_guard<std::mutex> lk(p_->mu);
  int fi = run.font_index;
  if (fi < 0 || fi >= static_cast<int>(p_->faces.size())) fi = 0;
  hb_buffer_t* buf = p_->buffers[fi];
  hb_buffer_reset(buf);
  hb_buffer_set_direction(buf, run.rtl ? HB_DIRECTION_RTL : HB_DIRECTION_LTR);
  if (!run.lang.empty()) {
    hb_language_t lang = hb_language_from_string(run.lang.c_str(), -1);
    hb_buffer_set_language(buf, lang);
  }
  hb_buffer_add_utf8(buf, run.text.data(), static_cast<int>(run.text.size()), 0, -1);
  hb_shape(p_->hb_fonts[fi], buf, nullptr, 0);
  unsigned int n = 0;
  hb_glyph_info_t* info = hb_buffer_get_glyph_infos(buf, &n);
  hb_glyph_position_t* pos = hb_buffer_get_glyph_positions(buf, &n);
  FT_Face face = p_->faces[fi];
  for (unsigned int i = 0; i < n; ++i) {
    GlyphBox g;
    g.glyph = info[i].codepoint;
    g.cluster = run.byte_off + info[i].cluster;
    g.advance = pos[i].x_advance;
    g.font_index = fi;
    if (FT_Load_Glyph(face, g.glyph, FT_LOAD_NO_HINTING) == 0) {
      FT_GlyphSlot s = face->glyph;
      g.bearing_x = s->metrics.horiBearingX;
      g.bearing_y = s->metrics.horiBearingY;
      g.width = s->metrics.width;
      g.height = s->metrics.height;
    }
    out.push_back(g);
  }
}

fixed_t FontChain::ascender() const { return p_->asc; }
fixed_t FontChain::descender() const { return p_->desc; }
fixed_t FontChain::line_height() const { return p_->line_h; }
fixed_t FontChain::missing_advance() const { return p_->missing_adv; }
int FontChain::size_px() const { return p_->px; }

}  // namespace sublay

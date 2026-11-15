#pragma once

#include <filesystem>
#include <stdexcept>
#include <string>
#include <vector>

#include "kernel.h"

namespace lutgrade {

struct CubeLut {
  int shaper_size = 0;
  int size_3d = 0;
  Vec3 domain_min{0.0, 0.0, 0.0};
  Vec3 domain_max{1.0, 1.0, 1.0};
  std::vector<Vec3> shaper;
  std::vector<Vec3> table;

  bool has_shaper() const { return shaper_size > 0; }
  bool has_3d() const { return size_3d > 0; }
};

class CubeError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

CubeLut parse_cube_file(const std::filesystem::path& path);

inline Vec3 normalize_domain(const CubeLut& lut, const Vec3& color) {
  const Vec3 span = lut.domain_max - lut.domain_min;
  return clamp01({(color.r - lut.domain_min.r) / span.r,
                  (color.g - lut.domain_min.g) / span.g,
                  (color.b - lut.domain_min.b) / span.b});
}

inline Vec3 apply_lut(const CubeLut& lut, const Vec3& color, Interpolation interp) {
  Vec3 value = normalize_domain(lut, color);
  if (lut.has_shaper()) {
    const auto shaper_at = [&lut](int index) { return lut.shaper[static_cast<std::size_t>(index)]; };
    value = sample_shaper(shaper_at, lut.shaper_size, value);
  }
  if (lut.has_3d()) {
    const auto table_at = [&lut](int r, int g, int b) {
      return lut.table[cube_index(lut.size_3d, r, g, b)];
    };
    if (interp == Interpolation::Tetrahedral) {
      value = sample_tetrahedral(table_at, lut.size_3d, value);
    } else {
      value = sample_trilinear(table_at, lut.size_3d, value);
    }
  }
  return value;
}

}  // namespace lutgrade


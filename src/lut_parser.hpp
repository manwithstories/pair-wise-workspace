#pragma once
#include <array>
#include <cstddef>
#include <string>
#include <vector>

#include "error.hpp"

// Strict parser for 3D .cube LUT files.
//
// Accepted grammar (one directive per line, '#' starts a comment):
//
//   TITLE "name"                optional, at most once
//   LUT_3D_SIZE N               required exactly once, N in {17, 33, 65}
//   DOMAIN_MIN r g b            optional, at most once, default 0 0 0
//   DOMAIN_MAX r g b            optional, at most once, default 1 1 1
//   LUT_3D_INPUT_RANGE lo hi    optional alias for min/max on all channels
//   r g b                       exactly N*N*N data lines
//
// Data is stored in file order, which the .cube convention defines as red
// varying fastest:
//   index = r + g*N + b*N*N
//
// Rejected with a descriptive Error: missing/duplicated declarations, a
// missing or surplus data line, an unsupported grid size, non-finite or
// malformed numbers, data outside [0, 1], and a domain whose max is not
// strictly above its min. `origin` is used only for message prefixes.
namespace lutgrade {

inline constexpr std::array<int, 3> kSupportedGridSizes{17, 33, 65};

struct Lut3D {
  std::string title;
  int size = 0;
  std::array<double, 3> domain_min{0.0, 0.0, 0.0};
  std::array<double, 3> domain_max{1.0, 1.0, 1.0};
  // size^3 interleaved RGB triplets, red-fastest file order.
  std::vector<std::array<float, 3>> table;

  std::size_t entry_count() const noexcept {
    return static_cast<std::size_t>(size) * size * size;
  }
};

Lut3D parse_cube(const std::string& text, const std::string& origin);
Lut3D load_cube(const std::string& path);

}  // namespace lutgrade

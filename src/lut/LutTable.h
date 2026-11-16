#pragma once
#include <cstddef>
#include <vector>

namespace lg {

// 3D LUT normalized to r-fastest ordering:
// node (r,g,b) lives at index ((b * size + g) * size + r) * 3.
struct LutTable {
  int size = 0;                          // grid dimension N
  double dmin[3] = {0.0, 0.0, 0.0};      // input domain lower bound
  double dmax[3] = {1.0, 1.0, 1.0};      // input domain upper bound
  std::vector<float> data;               // 3 floats per node

  std::size_t entries() const { return data.size() / 3; }

  const float* node(int r, int g, int b) const {
    return &data[(static_cast<std::size_t>(b * size + g) * size + r) * 3];
  }
  float* node(int r, int g, int b) {
    return &data[(static_cast<std::size_t>(b * size + g) * size + r) * 3];
  }
};

} // namespace lg

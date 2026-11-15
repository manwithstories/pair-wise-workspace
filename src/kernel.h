#pragma once

#include <algorithm>
#include <cmath>
#include <cstddef>

namespace lutgrade {

struct Vec3 {
  double r = 0.0;
  double g = 0.0;
  double b = 0.0;

  double& operator[](std::size_t i) { return i == 0 ? r : (i == 1 ? g : b); }
  double operator[](std::size_t i) const { return i == 0 ? r : (i == 1 ? g : b); }
};

inline Vec3 operator+(const Vec3& a, const Vec3& b) {
  return {a.r + b.r, a.g + b.g, a.b + b.b};
}

inline Vec3 operator-(const Vec3& a, const Vec3& b) {
  return {a.r - b.r, a.g - b.g, a.b - b.b};
}

inline Vec3 operator*(const Vec3& a, double s) {
  return {a.r * s, a.g * s, a.b * s};
}

inline Vec3 operator/(const Vec3& a, double s) {
  return {a.r / s, a.g / s, a.b / s};
}

inline double clamp01(double value) {
  return std::min(1.0, std::max(0.0, value));
}

inline Vec3 clamp01(const Vec3& value) {
  return {clamp01(value.r), clamp01(value.g), clamp01(value.b)};
}

inline Vec3 mix(const Vec3& a, const Vec3& b, double t) {
  return a + (b - a) * t;
}

enum class Interpolation {
  Trilinear,
  Tetrahedral,
};

inline std::size_t cube_index(int size, int r, int g, int b) {
  return static_cast<std::size_t>(r) +
         static_cast<std::size_t>(size) * static_cast<std::size_t>(g) +
         static_cast<std::size_t>(size) * static_cast<std::size_t>(size) *
             static_cast<std::size_t>(b);
}

template <typename Accessor>
Vec3 sample_trilinear(const Accessor& at, int size, const Vec3& position) {
  if (size <= 1) {
    return at(0, 0, 0);
  }

  const Vec3 p = clamp01(position);
  const double scale = static_cast<double>(size - 1);
  const double sr = p.r * scale;
  const double sg = p.g * scale;
  const double sb = p.b * scale;

  const int r0 = std::min(static_cast<int>(std::floor(sr)), size - 2);
  const int g0 = std::min(static_cast<int>(std::floor(sg)), size - 2);
  const int b0 = std::min(static_cast<int>(std::floor(sb)), size - 2);
  const double fr = sr - static_cast<double>(r0);
  const double fg = sg - static_cast<double>(g0);
  const double fb = sb - static_cast<double>(b0);

  const Vec3 c000 = at(r0, g0, b0);
  const Vec3 c100 = at(r0 + 1, g0, b0);
  const Vec3 c010 = at(r0, g0 + 1, b0);
  const Vec3 c001 = at(r0, g0, b0 + 1);
  const Vec3 c110 = at(r0 + 1, g0 + 1, b0);
  const Vec3 c101 = at(r0 + 1, g0, b0 + 1);
  const Vec3 c011 = at(r0, g0 + 1, b0 + 1);
  const Vec3 c111 = at(r0 + 1, g0 + 1, b0 + 1);

  const Vec3 c00 = mix(c000, c100, fr);
  const Vec3 c01 = mix(c001, c101, fr);
  const Vec3 c10 = mix(c010, c110, fr);
  const Vec3 c11 = mix(c011, c111, fr);
  return mix(mix(c00, c10, fg), mix(c01, c11, fg), fb);
}

template <typename Accessor>
Vec3 sample_tetrahedral(const Accessor& at, int size, const Vec3& position) {
  if (size <= 1) {
    return at(0, 0, 0);
  }

  const Vec3 p = clamp01(position);
  const double scale = static_cast<double>(size - 1);
  const double sr = p.r * scale;
  const double sg = p.g * scale;
  const double sb = p.b * scale;

  const int r0 = std::min(static_cast<int>(std::floor(sr)), size - 2);
  const int g0 = std::min(static_cast<int>(std::floor(sg)), size - 2);
  const int b0 = std::min(static_cast<int>(std::floor(sb)), size - 2);
  const double fr = sr - static_cast<double>(r0);
  const double fg = sg - static_cast<double>(g0);
  const double fb = sb - static_cast<double>(b0);

  const Vec3 c000 = at(r0, g0, b0);
  const Vec3 c100 = at(r0 + 1, g0, b0);
  const Vec3 c010 = at(r0, g0 + 1, b0);
  const Vec3 c001 = at(r0, g0, b0 + 1);
  const Vec3 c110 = at(r0 + 1, g0 + 1, b0);
  const Vec3 c101 = at(r0 + 1, g0, b0 + 1);
  const Vec3 c011 = at(r0, g0 + 1, b0 + 1);
  const Vec3 c111 = at(r0 + 1, g0 + 1, b0 + 1);

  if (fr >= fg) {
    if (fg >= fb) {
      return c000 + (c100 - c000) * fr + (c110 - c100) * fg + (c111 - c110) * fb;
    }
    if (fr >= fb) {
      return c000 + (c100 - c000) * fr + (c101 - c100) * fb + (c111 - c101) * fg;
    }
    return c000 + (c001 - c000) * fb + (c101 - c001) * fr + (c111 - c101) * fg;
  }
  if (fb > fg) {
    return c000 + (c001 - c000) * fb + (c011 - c001) * fg + (c111 - c011) * fr;
  }
  if (fb > fr) {
    return c000 + (c010 - c000) * fg + (c011 - c010) * fb + (c111 - c011) * fr;
  }
  return c000 + (c010 - c000) * fg + (c110 - c010) * fr + (c111 - c110) * fb;
}

template <typename Accessor>
Vec3 sample_shaper(const Accessor& at, int size, const Vec3& position) {
  if (size <= 1) {
    return at(0);
  }

  const Vec3 p = clamp01(position);
  const double scale = static_cast<double>(size - 1);
  Vec3 result;
  for (std::size_t channel = 0; channel < 3; ++channel) {
    const double s = p[channel] * scale;
    const int i0 = std::min(static_cast<int>(std::floor(s)), size - 2);
    const double fraction = s - static_cast<double>(i0);
    result[channel] = at(i0)[channel] + (at(i0 + 1)[channel] - at(i0)[channel]) * fraction;
  }
  return result;
}

}  // namespace lutgrade


#include "color_matrix.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cctype>

namespace lutgrade {
namespace {

struct Primaries {
  double rx, ry;
  double gx, gy;
  double bx, by;
  double wx, wy;
};

double determinant(const Mat3& matrix) {
  const auto& m = matrix.m;
  return m[0][0] * (m[1][1] * m[2][2] - m[1][2] * m[2][1]) -
         m[0][1] * (m[1][0] * m[2][2] - m[1][2] * m[2][0]) +
         m[0][2] * (m[1][0] * m[2][1] - m[1][1] * m[2][0]);
}

Mat3 invert_or_throw(const Mat3& matrix) {
  const double det = determinant(matrix);
  if (!std::isfinite(det) || std::abs(det) < 1e-12) {
    throw ColorError("matrix is singular and cannot be inverted");
  }

  Mat3 augmented;
  Mat3 result;
  for (int row = 0; row < 3; ++row) {
    for (int col = 0; col < 3; ++col) {
      augmented.m[row][col] = matrix.m[row][col];
      result.m[row][col] = row == col ? 1.0 : 0.0;
    }
  }

  for (int pivot = 0; pivot < 3; ++pivot) {
    int best = pivot;
    for (int row = pivot + 1; row < 3; ++row) {
      if (std::abs(augmented.m[row][pivot]) > std::abs(augmented.m[best][pivot])) {
        best = row;
      }
    }
    if (std::abs(augmented.m[best][pivot]) < 1e-12) {
      throw ColorError("matrix is singular and cannot be inverted");
    }
    for (int col = 0; col < 3; ++col) {
      std::swap(augmented.m[pivot][col], augmented.m[best][col]);
      std::swap(result.m[pivot][col], result.m[best][col]);
    }
    const double divisor = augmented.m[pivot][pivot];
    for (int col = 0; col < 3; ++col) {
      augmented.m[pivot][col] /= divisor;
      result.m[pivot][col] /= divisor;
    }
    for (int row = 0; row < 3; ++row) {
      if (row == pivot) {
        continue;
      }
      const double factor = augmented.m[row][pivot];
      for (int col = 0; col < 3; ++col) {
        augmented.m[row][col] -= factor * augmented.m[pivot][col];
        result.m[row][col] -= factor * result.m[pivot][col];
      }
    }
  }
  return result;
}

Vec3 white_xyz(double x, double y) {
  return {x / y, 1.0, (1.0 - x - y) / y};
}

Mat3 rgb_to_xyz(const Primaries& p) {
  Mat3 basis{{{p.rx / p.ry, p.gx / p.gy, p.bx / p.by},
              {1.0, 1.0, 1.0},
              {(1.0 - p.rx - p.ry) / p.ry, (1.0 - p.gx - p.gy) / p.gy,
               (1.0 - p.bx - p.by) / p.by}}};
  const Vec3 white = white_xyz(p.wx, p.wy);
  const Vec3 scale = multiply(invert_or_throw(basis), white);
  for (int col = 0; col < 3; ++col) {
    for (int row = 0; row < 3; ++row) {
      basis.m[row][col] *= scale[static_cast<std::size_t>(col)];
    }
  }
  return basis;
}

Primaries primaries_for(ColorSpaceId space) {
  switch (space) {
    case ColorSpaceId::Srgb:
    case ColorSpaceId::Rec709:
      return {0.640, 0.330, 0.300, 0.600, 0.150, 0.060, 0.3127, 0.3290};
    case ColorSpaceId::Rec2020:
      return {0.708, 0.292, 0.170, 0.797, 0.131, 0.046, 0.3127, 0.3290};
    case ColorSpaceId::DciP3:
      return {0.680, 0.320, 0.265, 0.690, 0.150, 0.060, 0.3140, 0.3510};
    case ColorSpaceId::AcesCg:
      return {0.713, 0.293, 0.165, 0.830, 0.128, 0.044, 0.32168, 0.33767};
  }
  throw ColorError("unsupported color space");
}

Mat3 bradford_adaptation(const Primaries& source, const Primaries& destination) {
  const Mat3 bradford{{{0.8951, 0.2664, -0.1614},
                       {-0.7502, 1.7135, 0.0367},
                       {0.0389, -0.0685, 1.0296}}};
  const Vec3 source_cone = multiply(bradford, white_xyz(source.wx, source.wy));
  const Vec3 destination_cone = multiply(bradford, white_xyz(destination.wx, destination.wy));
  Mat3 scale;
  for (int i = 0; i < 3; ++i) {
    scale.m[i][i] = destination_cone[static_cast<std::size_t>(i)] /
                    source_cone[static_cast<std::size_t>(i)];
  }
  return multiply(invert_or_throw(bradford), multiply(scale, bradford));
}

double srgb_decode(double value) {
  if (value <= 0.04045) {
    return value / 12.92;
  }
  return std::pow((value + 0.055) / 1.055, 2.4);
}

double srgb_encode(double value) {
  if (value <= 0.0031308) {
    return value * 12.92;
  }
  return 1.055 * std::pow(value, 1.0 / 2.4) - 0.055;
}

double rec709_decode(double value) {
  if (value < 0.081) {
    return value / 4.5;
  }
  return std::pow((value + 0.099) / 1.099, 1.0 / 0.45);
}

double rec709_encode(double value) {
  if (value < 0.018) {
    return value * 4.5;
  }
  return 1.099 * std::pow(value, 0.45) - 0.099;
}

double rec2020_decode(double value) {
  constexpr double alpha = 1.09929682680944;
  constexpr double beta = 0.018053968510807;
  if (value < beta * 4.5) {
    return value / 4.5;
  }
  return std::pow((value + alpha - 1.0) / alpha, 1.0 / 0.45);
}

double rec2020_encode(double value) {
  constexpr double alpha = 1.09929682680944;
  constexpr double beta = 0.018053968510807;
  if (value < beta) {
    return value * 4.5;
  }
  return alpha * std::pow(value, 0.45) - (alpha - 1.0);
}

std::string normalized_name(const std::string& name) {
  std::string result;
  for (const unsigned char ch : name) {
    if (std::isalnum(ch)) {
      result.push_back(static_cast<char>(std::tolower(ch)));
    }
  }
  return result;
}

}  // namespace

Mat3 multiply(const Mat3& a, const Mat3& b) {
  Mat3 result{{{0.0, 0.0, 0.0}, {0.0, 0.0, 0.0}, {0.0, 0.0, 0.0}}};
  for (int row = 0; row < 3; ++row) {
    for (int col = 0; col < 3; ++col) {
      for (int k = 0; k < 3; ++k) {
        result.m[row][col] += a.m[row][k] * b.m[k][col];
      }
    }
  }
  return result;
}

Vec3 multiply(const Mat3& matrix, const Vec3& value) {
  return {matrix.m[0][0] * value.r + matrix.m[0][1] * value.g + matrix.m[0][2] * value.b,
          matrix.m[1][0] * value.r + matrix.m[1][1] * value.g + matrix.m[1][2] * value.b,
          matrix.m[2][0] * value.r + matrix.m[2][1] * value.g + matrix.m[2][2] * value.b};
}

Mat3 inverse(const Mat3& matrix) { return invert_or_throw(matrix); }

const char* color_space_name(ColorSpaceId space) {
  switch (space) {
    case ColorSpaceId::Srgb: return "sRGB";
    case ColorSpaceId::Rec709: return "Rec.709";
    case ColorSpaceId::Rec2020: return "Rec.2020";
    case ColorSpaceId::DciP3: return "DCI-P3";
    case ColorSpaceId::AcesCg: return "ACEScg";
  }
  return "unknown";
}

ColorSpaceId parse_color_space(const std::string& name) {
  const std::string key = normalized_name(name);
  if (key == "srgb") return ColorSpaceId::Srgb;
  if (key == "rec709" || key == "bt709") return ColorSpaceId::Rec709;
  if (key == "rec2020" || key == "bt2020") return ColorSpaceId::Rec2020;
  if (key == "dcip3") return ColorSpaceId::DciP3;
  if (key == "acescg" || key == "aces") return ColorSpaceId::AcesCg;
  throw ColorError("unknown color space: " + name);
}

Mat3 rgb_to_xyz_matrix(ColorSpaceId space) {
  return rgb_to_xyz(primaries_for(space));
}

Mat3 rgb_to_rgb_matrix(ColorSpaceId source, ColorSpaceId destination) {
  const Primaries src_primaries = primaries_for(source);
  const Primaries dst_primaries = primaries_for(destination);
  const Mat3 src_to_xyz = rgb_to_xyz(src_primaries);
  const Mat3 dst_to_xyz = rgb_to_xyz(dst_primaries);
  return multiply(invert_or_throw(dst_to_xyz),
                  multiply(bradford_adaptation(src_primaries, dst_primaries), src_to_xyz));
}

double decode_transfer(ColorSpaceId space, double encoded) {
  switch (space) {
    case ColorSpaceId::Srgb: return srgb_decode(encoded);
    case ColorSpaceId::Rec709: return rec709_decode(encoded);
    case ColorSpaceId::Rec2020: return rec2020_decode(encoded);
    case ColorSpaceId::DciP3: return std::pow(encoded, 2.6);
    case ColorSpaceId::AcesCg: return encoded;
  }
  throw ColorError("unsupported transfer function");
}

double encode_transfer(ColorSpaceId space, double linear) {
  switch (space) {
    case ColorSpaceId::Srgb: return srgb_encode(linear);
    case ColorSpaceId::Rec709: return rec709_encode(linear);
    case ColorSpaceId::Rec2020: return rec2020_encode(linear);
    case ColorSpaceId::DciP3: return std::pow(linear, 1.0 / 2.6);
    case ColorSpaceId::AcesCg: return linear;
  }
  throw ColorError("unsupported transfer function");
}

Vec3 decode_rgb(ColorSpaceId space, const Vec3& encoded) {
  return {decode_transfer(space, encoded.r), decode_transfer(space, encoded.g),
          decode_transfer(space, encoded.b)};
}

Vec3 encode_rgb(ColorSpaceId space, const Vec3& linear) {
  return {encode_transfer(space, linear.r), encode_transfer(space, linear.g),
          encode_transfer(space, linear.b)};
}

Vec3 convert_linear_rgb(ColorSpaceId source, ColorSpaceId destination, const Vec3& value) {
  static const std::array<Mat3, 25> conversion_cache = [] {
    std::array<Mat3, 25> cache{};
    for (int source = 0; source < 5; ++source) {
      for (int destination = 0; destination < 5; ++destination) {
        cache[static_cast<std::size_t>(source * 5 + destination)] =
            rgb_to_rgb_matrix(static_cast<ColorSpaceId>(source),
                              static_cast<ColorSpaceId>(destination));
      }
    }
    return cache;
  }();
  const int index = static_cast<int>(source) * 5 + static_cast<int>(destination);
  return multiply(conversion_cache[static_cast<std::size_t>(index)], value);
}

Vec3 convert_encoded_rgb(ColorSpaceId source, ColorSpaceId destination, const Vec3& value) {
  return encode_rgb(destination, convert_linear_rgb(source, destination, decode_rgb(source, value)));
}

}  // namespace lutgrade


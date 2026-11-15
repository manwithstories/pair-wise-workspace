#pragma once

#include <cstdint>
#include <stdexcept>
#include <string>

#include "kernel.h"

namespace lutgrade {

enum class ColorSpaceId {
  Srgb,
  Rec709,
  Rec2020,
  DciP3,
  AcesCg,
};

struct Mat3 {
  double m[3][3] = {{1.0, 0.0, 0.0}, {0.0, 1.0, 0.0}, {0.0, 0.0, 1.0}};
};

class ColorError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

Mat3 multiply(const Mat3& a, const Mat3& b);
Vec3 multiply(const Mat3& matrix, const Vec3& value);
Mat3 inverse(const Mat3& matrix);

const char* color_space_name(ColorSpaceId space);
ColorSpaceId parse_color_space(const std::string& name);
Mat3 rgb_to_xyz_matrix(ColorSpaceId space);
Mat3 rgb_to_rgb_matrix(ColorSpaceId source, ColorSpaceId destination);

double decode_transfer(ColorSpaceId space, double encoded);
double encode_transfer(ColorSpaceId space, double linear);
Vec3 decode_rgb(ColorSpaceId space, const Vec3& encoded);
Vec3 encode_rgb(ColorSpaceId space, const Vec3& linear);
Vec3 convert_linear_rgb(ColorSpaceId source, ColorSpaceId destination, const Vec3& value);
Vec3 convert_encoded_rgb(ColorSpaceId source, ColorSpaceId destination, const Vec3& value);

}  // namespace lutgrade


#include "color_matrix.hpp"

#include <algorithm>
#include <cctype>
#include <cmath>
#include <cstddef>

namespace lutgrade {
namespace {

// Conversion matrices, row-major, indexed [from][to]. Derived from the
// published primaries + D65/D60 white points via Bradford adaptation; kept
// as literals so results do not vary with the platform's libm or BLAS.
constexpr std::array<std::array<double, 9>, 4> kToSrgb = {{
    /* srgb    -> srgb    */ {1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0},
    /* rec709  -> srgb    */ {1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0},
    /* rec2020 -> srgb    */ {1.660491002108, -0.587641138789, -0.072849863320,
                              -0.124550474522, 1.132899897126, -0.008349422604,
                              -0.018150763355, -0.100578898008, 1.118729661363},
    /* acescg  -> srgb    */ {1.705050992658, -0.621792120657, -0.083258872001,
                              -0.130256417507, 1.140804736575, -0.010548319068,
                              -0.024003356805, -0.128968976065, 1.152972332870},
}};

// Everything routes through sRGB (i.e. through XYZ D65), so the table only
// needs the "to sRGB" half plus the inverse "from sRGB" half.
constexpr std::array<std::array<double, 9>, 4> kFromSrgb = {{
    /* srgb    -> srgb    */ {1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0},
    /* srgb    -> rec709  */ {1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0},
    /* srgb    -> rec2020 */ {0.627403895935, 0.329283038378, 0.043313065687,
                              0.069097289358, 0.919540395075, 0.011362315566,
                              0.016391438875, 0.088013307877, 0.895595253248},
    /* srgb    -> acescg  */ {0.613097402401, 0.339523146184, 0.047379451415,
                              0.070193722470, 0.916353879057, 0.013452398473,
                              0.020615592882, 0.109569772938, 0.869814634180},
}};

std::size_t index_of(ColorSpace space) {
  switch (space) {
    case ColorSpace::kSrgb:
      return 0;
    case ColorSpace::kRec709:
      return 1;
    case ColorSpace::kRec2020:
      return 2;
    case ColorSpace::kAcescg:
      return 3;
  }
  return 0;
}

std::string lowered(std::string s) {
  for (char& c : s) {
    c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
  }
  return s;
}

double srgb_decode(double v) {
  return v <= 0.04045 ? v / 12.92 : std::pow((v + 0.055) / 1.055, 2.4);
}
double srgb_encode(double v) {
  return v <= 0.0031308 ? 12.92 * v : 1.055 * std::pow(v, 1.0 / 2.4) - 0.055;
}
// ITU-R BT.709 OETF/EOTF.
double bt709_decode(double v) {
  return v < 0.081 ? v / 4.5 : std::pow((v + 0.099) / 1.099, 1.0 / 0.45);
}
double bt709_encode(double v) {
  return v < 0.018 ? 4.5 * v : 1.099 * std::pow(v, 0.45) - 0.099;
}

}  // namespace

Matrix3 Matrix3::identity() {
  Matrix3 out;
  out.m = {1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0};
  return out;
}

Matrix3 Matrix3::from_row_major(std::array<double, 9> values) {
  for (const double v : values) {
    if (!std::isfinite(v)) {
      throw Error(ExitCode::kColor, "matrix_invalid",
                  "conversion matrix contains a non-finite entry");
    }
  }
  Matrix3 out;
  out.m = values;
  return out;
}

Matrix3 multiply(const Matrix3& a, const Matrix3& b) {
  Matrix3 out;
  for (std::size_t r = 0; r < 3; ++r) {
    for (std::size_t c = 0; c < 3; ++c) {
      double sum = 0.0;
      for (std::size_t k = 0; k < 3; ++k) sum += a.m[r * 3 + k] * b.m[k * 3 + c];
      out.m[r * 3 + c] = sum;
    }
  }
  return out;
}

std::array<double, 3> apply(const Matrix3& m, const std::array<double, 3>& rgb) {
  std::array<double, 3> out{};
  for (std::size_t r = 0; r < 3; ++r) {
    out[r] = m.m[r * 3 + 0] * rgb[0] + m.m[r * 3 + 1] * rgb[1] +
             m.m[r * 3 + 2] * rgb[2];
  }
  return out;
}

ColorSpace parse_color_space(const std::string& name) {
  const std::string key = lowered(name);
  if (key == "srgb") return ColorSpace::kSrgb;
  if (key == "rec709" || key == "bt709") return ColorSpace::kRec709;
  if (key == "rec2020" || key == "bt2020") return ColorSpace::kRec2020;
  if (key == "acescg" || key == "aces") return ColorSpace::kAcescg;
  throw Error(ExitCode::kColor, "space_unknown",
              "unknown colour space '" + name +
                  "' (expected srgb, rec709, rec2020 or acescg)");
}

const char* color_space_name(ColorSpace space) {
  switch (space) {
    case ColorSpace::kSrgb:
      return "srgb";
    case ColorSpace::kRec709:
      return "rec709";
    case ColorSpace::kRec2020:
      return "rec2020";
    case ColorSpace::kAcescg:
      return "acescg";
  }
  return "srgb";
}

Matrix3 conversion_matrix(ColorSpace from, ColorSpace to) {
  if (from == to) return Matrix3::identity();
  const Matrix3 to_srgb =
      Matrix3::from_row_major(kToSrgb[index_of(from)]);
  const Matrix3 from_srgb =
      Matrix3::from_row_major(kFromSrgb[index_of(to)]);
  return multiply(from_srgb, to_srgb);
}

ClampResult clamp_linear(const std::array<double, 3>& linear) {
  ClampResult out;
  for (std::size_t i = 0; i < 3; ++i) {
    const double v = linear[i];
    if (!std::isfinite(v)) {
      out.non_finite = true;
      out.rgb[i] = 0.0;
      continue;
    }
    if (v < 0.0) {
      out.clipped_low = true;
      out.rgb[i] = 0.0;
    } else if (v > 1.0) {
      out.clipped_high = true;
      out.rgb[i] = 1.0;
    } else {
      out.rgb[i] = v;
    }
  }
  return out;
}

double decode_transfer(ColorSpace space, double encoded) {
  const double v = std::clamp(encoded, 0.0, 1.0);
  switch (space) {
    case ColorSpace::kSrgb:
      return srgb_decode(v);
    case ColorSpace::kRec709:
      return bt709_decode(v);
    case ColorSpace::kRec2020:
      return bt709_decode(v);  // same OETF family; 10/12-bit only differs
    case ColorSpace::kAcescg:
      return v;  // ACEScg is already linear
  }
  return v;
}

double encode_transfer(ColorSpace space, double linear) {
  const double v = std::clamp(linear, 0.0, 1.0);
  switch (space) {
    case ColorSpace::kSrgb:
      return srgb_encode(v);
    case ColorSpace::kRec709:
      return bt709_encode(v);
    case ColorSpace::kRec2020:
      return bt709_encode(v);
    case ColorSpace::kAcescg:
      return v;
  }
  return v;
}

const char* transfer_name(ColorSpace space) {
  switch (space) {
    case ColorSpace::kSrgb:
      return "srgb-eotf";
    case ColorSpace::kRec709:
      return "bt709-eotf";
    case ColorSpace::kRec2020:
      return "bt2020-eotf";
    case ColorSpace::kAcescg:
      return "linear";
  }
  return "linear";
}

}  // namespace lutgrade

#include "color/ColorSpace.h"
#include "Errors.h"
#include <algorithm>
#include <cmath>

namespace lg {

Mat3 matMul(const Mat3& x, const Mat3& y) {
  Mat3 r;
  for (int i = 0; i < 3; ++i)
    for (int j = 0; j < 3; ++j) {
      double s = 0.0;
      for (int k = 0; k < 3; ++k)
        s += x.a[i * 3 + k] * y.a[k * 3 + j];
      r.a[i * 3 + j] = s;
    }
  return r;
}

double matDet(const Mat3& m) {
  const double* a = m.a;
  return a[0] * (a[4] * a[8] - a[5] * a[7]) -
         a[1] * (a[3] * a[8] - a[5] * a[6]) +
         a[2] * (a[3] * a[7] - a[4] * a[6]);
}

Mat3 matInverse(const Mat3& m, const char* what) {
  double d = matDet(m);
  if (std::fabs(d) < 1e-12)
    throw InternalError(std::string("singular matrix: ") + what);
  const double* a = m.a;
  Mat3 r;
  r.a[0] = (a[4] * a[8] - a[5] * a[7]) / d;
  r.a[1] = (a[2] * a[7] - a[1] * a[8]) / d;
  r.a[2] = (a[1] * a[5] - a[2] * a[4]) / d;
  r.a[3] = (a[5] * a[6] - a[3] * a[8]) / d;
  r.a[4] = (a[0] * a[8] - a[2] * a[6]) / d;
  r.a[5] = (a[2] * a[3] - a[0] * a[5]) / d;
  r.a[6] = (a[3] * a[7] - a[4] * a[6]) / d;
  r.a[7] = (a[1] * a[6] - a[0] * a[7]) / d;
  r.a[8] = (a[0] * a[4] - a[1] * a[3]) / d;
  return r;
}

ColorSpaceId parseColorSpace(const std::string& name) {
  std::string s;
  for (char c : name)
    s += static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
  if (s == "rec709" || s == "bt709" || s == "709")
    return ColorSpaceId::Rec709;
  if (s == "rec2020" || s == "bt2020" || s == "2020")
    return ColorSpaceId::Rec2020;
  if (s == "srgb")
    return ColorSpaceId::SRgb;
  if (s == "acescg" || s == "ap1")
    return ColorSpaceId::ACEScg;
  throw ArgsError("unknown color space: " + name +
                  " (rec709|rec2020|srgb|acescg)");
}

const char* colorSpaceName(ColorSpaceId id) {
  switch (id) {
  case ColorSpaceId::Rec709: return "rec709";
  case ColorSpaceId::Rec2020: return "rec2020";
  case ColorSpaceId::SRgb: return "srgb";
  case ColorSpaceId::ACEScg: return "acescg";
  }
  return "?";
}

static double bt709Decode(double v) {
  return v < 0.081 ? v / 4.5 : std::pow((v + 0.099) / 1.099, 1.0 / 0.45);
}
static double bt709Encode(double v) {
  return v < 0.018 ? 4.5 * v : 1.099 * std::pow(v, 0.45) - 0.099;
}
static double srgbDecode(double v) {
  return v <= 0.04045 ? v / 12.92 : std::pow((v + 0.055) / 1.055, 2.4);
}
static double srgbEncode(double v) {
  return v <= 0.0031308 ? 12.92 * v : 1.055 * std::pow(v, 1.0 / 2.4) - 0.055;
}

double transferDecode(ColorSpaceId id, double v) {
  switch (id) {
  case ColorSpaceId::Rec709:
  case ColorSpaceId::Rec2020: return bt709Decode(v);
  case ColorSpaceId::SRgb: return srgbDecode(v);
  case ColorSpaceId::ACEScg: return v; // linear working space
  }
  return v;
}

double transferEncode(ColorSpaceId id, double v) {
  switch (id) {
  case ColorSpaceId::Rec709:
  case ColorSpaceId::Rec2020: return bt709Encode(std::max(v, 0.0));
  case ColorSpaceId::SRgb: return srgbEncode(std::max(v, 0.0));
  case ColorSpaceId::ACEScg: return v;
  }
  return v;
}

namespace {

struct Primaries {
  double xr, yr, xg, yg, xb, yb, xw, yw;
};

Primaries primariesOf(ColorSpaceId id) {
  switch (id) {
  case ColorSpaceId::Rec709:
  case ColorSpaceId::SRgb:
    return {0.640, 0.330, 0.300, 0.600, 0.150, 0.060, 0.3127, 0.3290};
  case ColorSpaceId::Rec2020:
    return {0.708, 0.292, 0.170, 0.797, 0.131, 0.046, 0.3127, 0.3290};
  case ColorSpaceId::ACEScg: // AP1 primaries, D60 white
    return {0.713, 0.293, 0.165, 0.830, 0.128, 0.044, 0.32168, 0.33767};
  }
  return {};
}

Mat3 computeRgbToXyz(ColorSpaceId id) {
  Primaries p = primariesOf(id);
  auto col = [](double x, double y, double* dst) {
    dst[0] = x / y;
    dst[1] = 1.0;
    dst[2] = (1.0 - x - y) / y;
  };
  double cr[3], cg[3], cb[3];
  col(p.xr, p.yr, cr);
  col(p.xg, p.yg, cg);
  col(p.xb, p.yb, cb);
  Mat3 prim{{cr[0], cg[0], cb[0],
             cr[1], cg[1], cb[1],
             cr[2], cg[2], cb[2]}};
  double w[3];
  col(p.xw, p.yw, w);
  Mat3 inv = matInverse(prim, "primary matrix");
  double s[3];
  for (int i = 0; i < 3; ++i)
    s[i] = inv.a[i * 3] * w[0] + inv.a[i * 3 + 1] * w[1] + inv.a[i * 3 + 2] * w[2];
  Mat3 m;
  for (int i = 0; i < 3; ++i)
    for (int j = 0; j < 3; ++j)
      m.a[i * 3 + j] = prim.a[i * 3 + j] * s[j];
  return m;
}

} // namespace

const Mat3& rgbToXyz(ColorSpaceId id) {
  static const Mat3 m709 = computeRgbToXyz(ColorSpaceId::Rec709);
  static const Mat3 m2020 = computeRgbToXyz(ColorSpaceId::Rec2020);
  static const Mat3 mSrgb = computeRgbToXyz(ColorSpaceId::SRgb);
  static const Mat3 mAces = computeRgbToXyz(ColorSpaceId::ACEScg);
  switch (id) {
  case ColorSpaceId::Rec709: return m709;
  case ColorSpaceId::Rec2020: return m2020;
  case ColorSpaceId::SRgb: return mSrgb;
  case ColorSpaceId::ACEScg: return mAces;
  }
  return m709;
}

const Mat3& xyzToRgb(ColorSpaceId id) {
  static const Mat3 m709 = matInverse(rgbToXyz(ColorSpaceId::Rec709), "rec709 xyz->rgb");
  static const Mat3 m2020 = matInverse(rgbToXyz(ColorSpaceId::Rec2020), "rec2020 xyz->rgb");
  static const Mat3 mSrgb = matInverse(rgbToXyz(ColorSpaceId::SRgb), "srgb xyz->rgb");
  static const Mat3 mAces = matInverse(rgbToXyz(ColorSpaceId::ACEScg), "acescg xyz->rgb");
  switch (id) {
  case ColorSpaceId::Rec709: return m709;
  case ColorSpaceId::Rec2020: return m2020;
  case ColorSpaceId::SRgb: return mSrgb;
  case ColorSpaceId::ACEScg: return mAces;
  }
  return m709;
}

void convertPixel(ColorSpaceId from, ColorSpaceId to, const double in[3],
                  double out[3]) {
  if (from == to) {
    for (int c = 0; c < 3; ++c)
      out[c] = in[c];
    return;
  }
  double lin[3];
  for (int c = 0; c < 3; ++c)
    lin[c] = transferDecode(from, in[c]);
  Mat3 m = matMul(xyzToRgb(to), rgbToXyz(from));
  double v[3];
  for (int i = 0; i < 3; ++i)
    v[i] = m.a[i * 3] * lin[0] + m.a[i * 3 + 1] * lin[1] + m.a[i * 3 + 2] * lin[2];
  for (int c = 0; c < 3; ++c)
    out[c] = transferEncode(to, v[c]);
}

} // namespace lg

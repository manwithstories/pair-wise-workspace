#include "interpolator.hpp"

#include <algorithm>
#include <cmath>
#include <sstream>

namespace lutgrade {
namespace {

double clamp01(double v) { return v < 0.0 ? 0.0 : (v > 1.0 ? 1.0 : v); }

[[noreturn]] void render_fail(const std::string& what) {
  throw Error(ExitCode::kRender, "render_failed", what);
}

// Maps one encoded channel through the LUT domain into cube coordinates
// [0, size-1] and returns the two grid indices plus the interpolation weight.
struct Axis {
  std::size_t lo = 0;
  std::size_t hi = 0;
  double frac = 0.0;
};

Axis axis_for(const Lut3D& lut, double encoded, std::size_t channel) {
  if (!std::isfinite(encoded)) {
    throw Error(ExitCode::kRender, "interp_non_finite",
                "non-finite channel entering the LUT");
  }
  const double lo_domain = lut.domain_min[channel];
  const double hi_domain = lut.domain_max[channel];
  const double span = hi_domain - lo_domain;
  double t = (encoded - lo_domain) / span;
  // Boundary clamp: anything outside the declared domain samples the edge.
  t = clamp01(t);
  const double scaled = t * static_cast<double>(lut.size - 1);
  const std::size_t last = static_cast<std::size_t>(lut.size - 1);
  std::size_t i0 = static_cast<std::size_t>(std::floor(scaled));
  if (i0 >= last) i0 = last - 1;  // size >= 17, so this never underflows
  Axis axis;
  axis.lo = i0;
  axis.hi = i0 + 1;
  axis.frac = scaled - static_cast<double>(i0);
  return axis;
}

std::array<float, 3> mix(const std::array<float, 3>& a,
                         const std::array<float, 3>& b, double t) {
  const double u = 1.0 - t;
  return {static_cast<float>(u * a[0] + t * b[0]),
          static_cast<float>(u * a[1] + t * b[1]),
          static_cast<float>(u * a[2] + t * b[2])};
}

}  // namespace

std::array<float, 3> lut_sample_trilinear(const Lut3D& lut,
                                          const std::array<double, 3>& encoded) {
  if (lut.size < 2 || lut.table.size() != lut.entry_count()) {
    render_fail("LUT table is inconsistent with its declared size");
  }
  const Axis ax = axis_for(lut, encoded[0], 0);
  const Axis ay = axis_for(lut, encoded[1], 1);
  const Axis az = axis_for(lut, encoded[2], 2);

  const std::size_t n = static_cast<std::size_t>(lut.size);
  // Red varies fastest: index = r + g*N + b*N*N.
  auto at = [&](std::size_t r, std::size_t g, std::size_t b) -> const std::array<float, 3>& {
    return lut.table[r + g * n + b * n * n];
  };

  // Trilinear: blend along R, then G, then B.
  const auto c000 = at(ax.lo, ay.lo, az.lo);
  const auto c100 = at(ax.hi, ay.lo, az.lo);
  const auto c010 = at(ax.lo, ay.hi, az.lo);
  const auto c110 = at(ax.hi, ay.hi, az.lo);
  const auto c001 = at(ax.lo, ay.lo, az.hi);
  const auto c101 = at(ax.hi, ay.lo, az.hi);
  const auto c011 = at(ax.lo, ay.hi, az.hi);
  const auto c111 = at(ax.hi, ay.hi, az.hi);

  const auto c00 = mix(c000, c100, ax.frac);
  const auto c10 = mix(c010, c110, ax.frac);
  const auto c01 = mix(c001, c101, ax.frac);
  const auto c11 = mix(c011, c111, ax.frac);
  const auto c0 = mix(c00, c10, ay.frac);
  const auto c1 = mix(c01, c11, ay.frac);
  return mix(c0, c1, az.frac);
}

GradingKernel::GradingKernel(const Lut3D* lut, GradeSettings settings)
    : lut_(lut), settings_(settings) {
  if (lut_ == nullptr) render_fail("grading kernel requires a LUT");
  settings_.matrix = conversion_matrix(settings_.in_space, settings_.out_space);
}

PixelResult GradingKernel::process(std::uint8_t r, std::uint8_t g, std::uint8_t b,
                                   int x, int y) const {
  PixelResult out;

  // 1-2. Normalise and decode the source transfer function.
  std::array<double, 3> linear{};
  const double encoded[3] = {r / 255.0, g / 255.0, b / 255.0};
  for (std::size_t i = 0; i < 3; ++i) {
    linear[i] = decode_transfer(settings_.in_space, encoded[i]);
  }

  // 3. Primary conversion in linear light. Qualified to dodge ADL picking up
  // std::apply from the std::array argument.
  const auto converted = lutgrade::apply(settings_.matrix, linear);

  // 4. Clamp and record out-of-gamut.
  const ClampResult clamped = clamp_linear(converted);
  if (clamped.non_finite) {
    // A non-finite value here is a bug in the matrix path, not a data issue.
    std::ostringstream msg;
    msg << "non-finite linear value at (" << x << ',' << y
        << ") after matrix conversion";
    render_fail(msg.str());
  }
  if (clamped.clipped_low || clamped.clipped_high) out.clipped = true;

  // 5. Encode with the destination transfer function.
  std::array<double, 3> encoded_out{};
  for (std::size_t i = 0; i < 3; ++i) {
    encoded_out[i] = encode_transfer(settings_.out_space, clamped.rgb[i]);
    out.pre_lut[i] = static_cast<float>(encoded_out[i]);
  }

  // 6. Trilinear LUT sample in the destination-encoded domain.
  std::array<float, 3> sampled{};
  try {
    sampled = lut_sample_trilinear(*lut_, encoded_out);
  } catch (const Error& e) {
    std::ostringstream msg;
    msg << "LUT sampling failed at (" << x << ',' << y << "): " << e.message();
    render_fail(msg.str());
  }

  // 7. Clamp the LUT result and detect non-finite.
  for (std::size_t i = 0; i < 3; ++i) {
    const float v = sampled[i];
    if (!std::isfinite(v)) {
      out.non_finite = true;
      out.rgb[i] = 0.0f;
      continue;
    }
    const double clamped_v = clamp01(static_cast<double>(v));
    if (clamped_v != static_cast<double>(v)) out.clipped = true;
    out.rgb[i] = static_cast<float>(clamped_v);
  }
  if (out.non_finite) {
    std::ostringstream msg;
    msg << "non-finite LUT result at (" << x << ',' << y << ')';
    render_fail(msg.str());
  }
  return out;
}

Image quantize(const RenderOutput& rendered) {
  Image image;
  image.width = rendered.width;
  image.height = rendered.height;
  image.rgb.resize(rendered.pixel_count() * 3);
  for (std::size_t i = 0; i < image.rgb.size(); ++i) {
    const double v = rendered.after[i];
    double scaled = v * 255.0;
    if (!std::isfinite(scaled)) scaled = 0.0;
    scaled = std::floor(scaled + 0.5);  // half away from zero for >= 0
    if (scaled < 0.0) scaled = 0.0;
    if (scaled > 255.0) scaled = 255.0;
    image.rgb[i] = static_cast<std::uint8_t>(scaled);
  }
  return image;
}

}  // namespace lutgrade

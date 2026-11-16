#pragma once
#include "image_io.hpp"
#include "interpolator.hpp"

// Single-threaded render path.
//
// Iterates every pixel in row-major order and calls GradingKernel::process.
// This is the reference implementation the tiled path is compared against;
// its output ordering is therefore trivially the input ordering.
namespace lutgrade {

RenderOutput render_scalar(const Image& input, const GradingKernel& kernel);

}  // namespace lutgrade

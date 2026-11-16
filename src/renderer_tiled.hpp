#pragma once
#include <cstddef>

#include "image_io.hpp"
#include "interpolator.hpp"

// Tiled, multi-threaded render path.
//
// The image is cut into fixed-size tiles (independent of thread count) and the
// tiles are handed to a pool of `threads` workers. Each tile writes only to its
// own pixels, so the output is byte-identical to the scalar path for any thread
// count — parallelism changes scheduling, never arithmetic.
//
// Failure semantics: if any tile throws, the first failure (by tile order, not
// by arrival time) is recorded, every worker stops at its next tile boundary,
// and the whole render throws Error(kRender). No partial image is produced.
namespace lutgrade {

inline constexpr int kTileSize = 32;

struct TiledOptions {
  int threads = 1;  // caller-validated; 1 runs the same tiling on this thread
};

RenderOutput render_tiled(const Image& input, const GradingKernel& kernel,
                          const TiledOptions& options);

}  // namespace lutgrade

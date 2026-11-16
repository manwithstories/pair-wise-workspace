#include "renderer_tiled.hpp"

#include <algorithm>
#include <atomic>
#include <exception>
#include <mutex>
#include <thread>
#include <vector>

namespace lutgrade {
namespace {

struct Tile {
  int x0 = 0;
  int y0 = 0;
  int x1 = 0;
  int y1 = 0;
  std::size_t index = 0;
};

std::vector<Tile> make_tiles(int width, int height) {
  std::vector<Tile> tiles;
  const int cols = (width + kTileSize - 1) / kTileSize;
  const int rows = (height + kTileSize - 1) / kTileSize;
  tiles.reserve(static_cast<std::size_t>(cols) * rows);
  for (int ty = 0; ty < rows; ++ty) {
    for (int tx = 0; tx < cols; ++tx) {
      Tile tile;
      tile.x0 = tx * kTileSize;
      tile.y0 = ty * kTileSize;
      tile.x1 = std::min(width, tile.x0 + kTileSize);
      tile.y1 = std::min(height, tile.y0 + kTileSize);
      tiles.push_back(tile);
    }
  }
  // Stable numbering so "first failure by tile order" is deterministic.
  for (std::size_t i = 0; i < tiles.size(); ++i) tiles[i].index = i;
  return tiles;
}

struct SharedState {
  const Image* input = nullptr;
  const GradingKernel* kernel = nullptr;
  RenderOutput* out = nullptr;
  const std::vector<Tile>* tiles = nullptr;
  std::atomic<std::size_t> next{0};
  std::atomic<bool> stop{false};

  std::mutex err_mutex;
  bool has_error = false;
  std::size_t error_tile = 0;
  ExitCode error_code = ExitCode::kRender;
  std::string error_kind;
  std::string error_message;
};

void record_error(SharedState& state, const Tile& tile, const Error& error) {
  std::lock_guard<std::mutex> lock(state.err_mutex);
  // Prefer the earliest tile so the reported failure does not depend on which
  // worker happened to win the race.
  if (!state.has_error || tile.index < state.error_tile) {
    state.has_error = true;
    state.error_tile = tile.index;
    state.error_code = error.code();
    state.error_kind = error.kind();
    state.error_message = error.message();
  }
  state.stop.store(true, std::memory_order_relaxed);
}

void run_tile(SharedState& state, const Tile& tile) {
  const Image& input = *state.input;
  RenderOutput& out = *state.out;
  std::uint64_t clipped = 0;
  for (int y = tile.y0; y < tile.y1; ++y) {
    for (int x = tile.x0; x < tile.x1; ++x) {
      const std::size_t p = static_cast<std::size_t>(y) * input.width + x;
      const std::uint8_t* src = input.rgb.data() + p * 3;
      const PixelResult result = state.kernel->process(src[0], src[1], src[2], x, y);
      out.before[p * 3 + 0] = result.pre_lut[0];
      out.before[p * 3 + 1] = result.pre_lut[1];
      out.before[p * 3 + 2] = result.pre_lut[2];
      out.after[p * 3 + 0] = result.rgb[0];
      out.after[p * 3 + 1] = result.rgb[1];
      out.after[p * 3 + 2] = result.rgb[2];
      if (result.clipped) ++clipped;
    }
  }
  // Accumulate clipped count under the mutex; one tiny critical section per
  // tile keeps the counter deterministic regardless of scheduling.
  {
    std::lock_guard<std::mutex> lock(state.err_mutex);
    out.clipped_pixels += clipped;
  }
}

void worker(SharedState& state) {
  for (;;) {
    if (state.stop.load(std::memory_order_relaxed)) return;
    const std::size_t i = state.next.fetch_add(1, std::memory_order_relaxed);
    if (i >= state.tiles->size()) return;
    const Tile& tile = (*state.tiles)[i];
    try {
      run_tile(state, tile);
    } catch (const Error& e) {
      record_error(state, tile, e);
      return;
    } catch (const std::exception& e) {
      Error wrapped(ExitCode::kRender, "render_failed", e.what());
      record_error(state, tile, wrapped);
      return;
    } catch (...) {
      Error wrapped(ExitCode::kRender, "render_failed",
                    "unknown failure in render worker");
      record_error(state, tile, wrapped);
      return;
    }
  }
}

}  // namespace

RenderOutput render_tiled(const Image& input, const GradingKernel& kernel,
                          const TiledOptions& options) {
  if (options.threads < 1) {
    throw Error(ExitCode::kUsage, "threads_invalid",
                "thread count must be at least 1");
  }
  const std::size_t pixels = input.pixel_count();
  RenderOutput out;
  out.width = input.width;
  out.height = input.height;
  out.before.resize(pixels * 3);
  out.after.resize(pixels * 3);

  const std::vector<Tile> tiles = make_tiles(input.width, input.height);
  SharedState state;
  state.input = &input;
  state.kernel = &kernel;
  state.out = &out;
  state.tiles = &tiles;

  const int worker_count =
      std::min(options.threads, static_cast<int>(tiles.size()));
  if (worker_count <= 1) {
    // Still goes through the pool logic so failure reporting matches the
    // multi-threaded case exactly.
    worker(state);
  } else {
    std::vector<std::thread> threads;
    threads.reserve(static_cast<std::size_t>(worker_count));
    for (int i = 0; i < worker_count; ++i) threads.emplace_back(worker, std::ref(state));
    for (auto& t : threads) t.join();
  }

  if (state.has_error) {
    throw Error(state.error_code, state.error_kind,
                state.error_message + " [tile " + std::to_string(state.error_tile) +
                    "; render aborted, no output written]");
  }
  return out;
}

}  // namespace lutgrade

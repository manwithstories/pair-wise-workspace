#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <iomanip>
#include <random>
#include <sstream>
#include <stdexcept>
#include <string>
#include <system_error>
#include <vector>

#include "image_io.h"
#include "kernel.h"

namespace lutgrade {
namespace {

std::atomic<std::uint64_t> staging_serial{0};

double seeded_noise(std::mt19937& random) {
  std::uniform_real_distribution<double> distribution(-0.00008, 0.00008);
  return distribution(random);
}

FloatImage make_log_ramp(std::uint32_t width, std::uint32_t height, std::uint32_t seed) {
  FloatImage image;
  image.width = width;
  image.height = height;
  image.channels = 3;
  image.pixels.resize(static_cast<std::size_t>(width) * height * 3);
  std::mt19937 random(seed);

  const std::uint32_t block_start = height * 3U / 4U;
  for (std::uint32_t y = 0; y < height; ++y) {
    for (std::uint32_t x = 0; x < width; ++x) {
      const double u = width > 1 ? static_cast<double>(x) / (width - 1) : 0.0;
      const double v = height > 1 ? static_cast<double>(y) / (height - 1) : 0.0;
      Vec3 color;
      if (y >= block_start) {
        const unsigned int block = std::min(7U, x * 8U / width);
        const double colors[8][3] = {{0.95, 0.09, 0.07}, {0.08, 0.76, 0.18},
                                     {0.05, 0.18, 0.92}, {0.92, 0.86, 0.06},
                                     {0.09, 0.78, 0.83}, {0.84, 0.10, 0.72},
                                     {0.93, 0.93, 0.93}, {0.035, 0.035, 0.035}};
        color = {colors[block][0], colors[block][1], colors[block][2]};
      } else {
        const double log_linear = std::pow(10.0, -2.2 + 2.2 * u);
        const double shoulder = 0.92 + 0.08 * std::sin(v * 3.141592653589793);
        color = {log_linear * shoulder, log_linear * (0.94 + 0.05 * v),
                 log_linear * (0.90 + 0.07 * (1.0 - v))};
      }
      const double noise = seeded_noise(random);
      image.at(x, y, 0) = static_cast<float>(clamp01(color.r + noise));
      image.at(x, y, 1) = static_cast<float>(clamp01(color.g + noise));
      image.at(x, y, 2) = static_cast<float>(clamp01(color.b + noise));
    }
  }
  return image;
}

FloatImage make_color_bars(std::uint32_t width, std::uint32_t height, std::uint32_t seed) {
  FloatImage image;
  image.width = width;
  image.height = height;
  image.channels = 3;
  image.pixels.resize(static_cast<std::size_t>(width) * height * 3);
  std::mt19937 random(seed ^ 0x9e3779b9U);
  const double colors[8][3] = {{0.95, 0.08, 0.06}, {0.08, 0.80, 0.18}, {0.05, 0.16, 0.95},
                               {0.95, 0.90, 0.05}, {0.05, 0.80, 0.88}, {0.88, 0.08, 0.72},
                               {0.95, 0.95, 0.95}, {0.02, 0.02, 0.02}};
  for (std::uint32_t y = 0; y < height; ++y) {
    for (std::uint32_t x = 0; x < width; ++x) {
      const unsigned int block = std::min(7U, x * 8U / width);
      const double shade = 0.18 + 0.82 * (height > 1 ? static_cast<double>(y) / (height - 1) : 1.0);
      for (std::uint8_t channel = 0; channel < 3; ++channel) {
        image.at(x, y, channel) = static_cast<float>(
            clamp01(colors[block][channel] * shade + seeded_noise(random)));
      }
    }
  }
  return image;
}

std::string make_cube(std::uint32_t seed, int shaper_size, int grid_size) {
  std::ostringstream cube;
  cube << std::scientific << std::setprecision(8);
  cube << "TITLE \"LUTGrade deterministic fixture\"\n";
  cube << "# Combined shaper and 3D look, seed=" << seed << "\n";
  cube << "LUT_1D_SIZE " << shaper_size << "\n";
  cube << "DOMAIN_MIN 0.0e0 0.0e0 0.0e0\n";
  cube << "DOMAIN_MAX 1.0e0 1.0e0 1.0e0\n";
  for (int i = 0; i < shaper_size; ++i) {
    const double t = static_cast<double>(i) / (shaper_size - 1);
    const double shaped = std::pow(t, 0.92);
    cube << shaped << ' ' << shaped << ' ' << shaped << '\n';
  }

  cube << "LUT_3D_SIZE " << grid_size << "\n";
  const double warm = static_cast<double>(static_cast<int>(seed % 7U) - 3) * 0.0025;
  for (int b = 0; b < grid_size; ++b) {
    for (int g = 0; g < grid_size; ++g) {
      for (int r = 0; r < grid_size; ++r) {
        const Vec3 source{static_cast<double>(r) / (grid_size - 1),
                          static_cast<double>(g) / (grid_size - 1),
                          static_cast<double>(b) / (grid_size - 1)};
        const double luma = 0.2126 * source.r + 0.7152 * source.g + 0.0722 * source.b;
        const double contrast = luma + (source.r - luma) * 1.08;
        const double midtone = 0.5 + (luma - 0.5) * 1.04;
        const Vec3 graded{clamp01(contrast * 1.035 + warm + (midtone - luma) * 0.2),
                          clamp01(source.g * 1.015 + warm * 0.3 + (midtone - luma) * 0.2),
                          clamp01(source.b * 0.965 - warm + (midtone - luma) * 0.2)};
        cube << graded.r << ' ' << graded.g << ' ' << graded.b << '\n';
      }
    }
  }
  return cube.str();
}

struct StagedFile {
  std::filesystem::path staged;
  std::filesystem::path final;
};

struct FileBackup {
  std::filesystem::path final;
  std::filesystem::path saved;
};

std::filesystem::path hidden_sibling(const std::filesystem::path& path, const char* tag) {
  const std::uint64_t serial = staging_serial.fetch_add(1, std::memory_order_relaxed);
  const auto ticks = std::chrono::steady_clock::now().time_since_epoch().count();
  return path.parent_path() /
         ("." + path.filename().string() + tag + std::to_string(ticks) + "." +
          std::to_string(serial));
}

void commit_staged_files(std::vector<StagedFile>& files) {
  std::vector<FileBackup> backups;
  std::vector<std::filesystem::path> committed;
  std::error_code error;
  try {
    for (const StagedFile& file : files) {
      if (!std::filesystem::exists(file.final, error)) {
        if (error) {
          throw ImageIoError("cannot inspect fixture output: " + error.message());
        }
        continue;
      }
      if (std::filesystem::is_directory(file.final, error)) {
        throw ImageIoError("fixture output path is a directory: " + file.final.string());
      }
      FileBackup backup{file.final, hidden_sibling(file.final, ".bak.")};
      std::filesystem::rename(file.final, backup.saved, error);
      if (error) {
        throw ImageIoError("cannot back up fixture output: " + error.message());
      }
      backups.push_back(backup);
    }
    for (const StagedFile& file : files) {
      std::filesystem::rename(file.staged, file.final, error);
      if (error) {
        throw ImageIoError("cannot commit fixture output: " + error.message());
      }
      committed.push_back(file.final);
    }
  } catch (...) {
    for (const auto& path : committed) {
      std::error_code ignored;
      std::filesystem::remove(path, ignored);
    }
    for (auto backup = backups.rbegin(); backup != backups.rend(); ++backup) {
      std::error_code ignored;
      std::filesystem::rename(backup->saved, backup->final, ignored);
    }
    throw;
  }
  for (const FileBackup& backup : backups) {
    std::error_code ignored;
    std::filesystem::remove(backup.saved, ignored);
  }
}

}  // namespace

void generate_fixture(std::uint32_t seed, std::uint32_t width, std::uint32_t height,
                      const std::filesystem::path& output_directory) {
  if (width == 0 || height == 0 || width > 65535 || height > 65535) {
    throw std::invalid_argument("fixture dimensions are out of range");
  }
  std::error_code error;
  const bool output_existed = std::filesystem::exists(output_directory, error);
  if (error) {
    throw ImageIoError("cannot inspect fixture directory: " + error.message());
  }
  std::filesystem::create_directories(output_directory, error);
  if (error) {
    throw ImageIoError("cannot create fixture directory: " + error.message());
  }
  const std::filesystem::path staging = output_directory / hidden_sibling("stage", ".tmp.");
  std::filesystem::create_directory(staging, error);
  if (error) {
    const std::string message = error.message();
    std::error_code ignored;
    if (!output_existed) std::filesystem::remove(output_directory, ignored);
    throw ImageIoError("cannot create fixture staging directory: " + message);
  }
  const std::filesystem::path ramp_path = output_directory / "log_ramp.ppm";
  const std::filesystem::path bars_path = output_directory / "color_bars.ppm";
  const std::filesystem::path cube_path = output_directory / "grade33.cube";
  try {
    write_image_atomic(staging / ramp_path.filename(), make_log_ramp(width, height, seed));
    write_image_atomic(staging / bars_path.filename(), make_color_bars(width, height, seed));
    write_text_atomic(staging / cube_path.filename(), make_cube(seed, 1024, 33));
  } catch (...) {
    std::filesystem::remove_all(staging, error);
    if (!output_existed) std::filesystem::remove(output_directory, error);
    throw;
  }

  std::vector<StagedFile> files{{staging / ramp_path.filename(), ramp_path},
                                {staging / bars_path.filename(), bars_path},
                                {staging / cube_path.filename(), cube_path}};
  try {
    commit_staged_files(files);
  } catch (...) {
    std::filesystem::remove_all(staging, error);
    if (!output_existed) std::filesystem::remove(output_directory, error);
    throw;
  }
  std::filesystem::remove_all(staging, error);
}

}  // namespace lutgrade


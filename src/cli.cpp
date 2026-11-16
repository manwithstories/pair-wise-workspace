#include "cli.hpp"

#include <sys/stat.h>

namespace lutgrade {
namespace {

bool is_regular_file(const std::string& path) {
  struct stat info {};
  if (::stat(path.c_str(), &info) != 0) return false;
  return S_ISREG(info.st_mode);
}

void require_readable(const std::string& path, const char* flag) {
  if (path.empty()) {
    throw Error(ExitCode::kUsage, "missing_argument",
                std::string(flag) + " is required");
  }
  if (!is_regular_file(path)) {
    throw Error(ExitCode::kUsage, "file_not_found",
                std::string(flag) + " is not a readable file: " + path);
  }
}

void validate_threads(int threads) {
  if (threads < 1 || threads > kMaxThreads) {
    throw Error(ExitCode::kUsage, "threads_invalid",
                "threads must be between 1 and " + std::to_string(kMaxThreads));
  }
}

std::string parent_dir(const std::string& path) {
  const std::size_t slash = path.find_last_of('/');
  if (slash == std::string::npos) return ".";
  if (slash == 0) return "/";
  return path.substr(0, slash);
}

// Shared by render and compare: files, threads, spaces.
void validate_color(ColorOptions& color) {
  require_readable(color.input, "--input");
  require_readable(color.lut, "--lut");
  validate_threads(color.threads);
  color.in_space = parse_color_space(color.in_space_text);
  color.out_space = parse_color_space(color.out_space_text);
}

}  // namespace

void validate_render(RenderOptions& options) {
  validate_color(options.color);
  if (options.path != "scalar" && options.path != "tiled") {
    throw Error(ExitCode::kUsage, "path_invalid",
                "--path must be 'scalar' or 'tiled', got '" + options.path + "'");
  }
  options.tiled = options.path == "tiled";

  if (options.output.empty()) {
    throw Error(ExitCode::kUsage, "missing_argument", "--output is required");
  }
  const std::string dir = parent_dir(options.output);
  struct stat info {};
  if (::stat(dir.c_str(), &info) != 0 || !S_ISDIR(info.st_mode)) {
    throw Error(ExitCode::kUsage, "output_dir_missing",
                "output directory does not exist: " + dir);
  }
  // A scalar render is single-threaded by definition; --threads only applies to
  // the tiled path. Reject the contradictory combination rather than silently
  // ignoring the flag.
  if (!options.tiled && options.color.threads != 1) {
    throw Error(ExitCode::kUsage, "threads_invalid",
                "--threads only applies to --path tiled");
  }
}

void validate_fixture(const FixtureOptions& options) {
  if (options.size != 17 && options.size != 33 && options.size != 65) {
    throw Error(ExitCode::kUsage, "grid_size_invalid",
                "--size must be 17, 33 or 65");
  }
  if (options.dir.empty()) {
    throw Error(ExitCode::kUsage, "missing_argument", "--dir is required");
  }
}

void validate_compare(CompareOptions& options) {
  validate_color(options.color);
  if (!(options.threshold > 0.0) || options.threshold > 1.0) {
    throw Error(ExitCode::kUsage, "threshold_invalid",
                "--threshold must be in (0, 1]");
  }
}

}  // namespace lutgrade

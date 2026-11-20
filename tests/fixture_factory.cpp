#include "fixture_factory.hpp"

#include <atomic>
#include <cstdio>
#include <fstream>
#include <random>
#include <system_error>

namespace mediaqc::testing {

TempDir::TempDir() {
  std::random_device device;
  std::uniform_int_distribution<std::uint64_t> pick;
  for (int attempt = 0; attempt < 100; ++attempt) {
    auto candidate = std::filesystem::temp_directory_path() /
                     ("mediaqc-test-" + std::to_string(pick(device)));
    std::error_code ec;
    if (std::filesystem::create_directory(candidate, ec) && !ec) {
      path_ = candidate;
      return;
    }
  }
  path_ = std::filesystem::temp_directory_path() / "mediaqc-test-fallback";
  std::filesystem::create_directories(path_);
}

TempDir::~TempDir() {
  if (path_.empty()) {
    return;
  }
  std::error_code ec;
  std::filesystem::remove_all(path_, ec);
}

bool TempDir::write(const std::string& name, std::string_view content) const {
  std::filesystem::path target = path_ / name;
  std::error_code ec;
  if (target.has_parent_path()) {
    std::filesystem::create_directories(target.parent_path(), ec);
  }
  std::ofstream out(target, std::ios::binary | std::ios::trunc);
  if (!out) {
    return false;
  }
  out.write(content.data(), static_cast<std::streamsize>(content.size()));
  return static_cast<bool>(out);
}

}  // namespace mediaqc::testing

#pragma once

#include <filesystem>
#include <string>
#include <string_view>

namespace mediaqc::testing {

// Creates a unique temporary directory that the fixture factory owns and
// removes.  Deliberately local to TMPDIR so tests never touch user data.
class TempDir {
 public:
  TempDir();
  ~TempDir();
  TempDir(const TempDir&) = delete;
  TempDir& operator=(const TempDir&) = delete;

  [[nodiscard]] const std::filesystem::path& path() const { return path_; }

  // Writes `content` to `name` inside the directory, creating parents.
  bool write(const std::string& name, std::string_view content) const;

 private:
  std::filesystem::path path_;
};

}  // namespace mediaqc::testing

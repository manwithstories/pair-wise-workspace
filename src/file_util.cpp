#include "media/file_util.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <unistd.h>

#include "media/audio.hpp"

namespace media::fsutil {

namespace fs = std::filesystem;

static fs::path lexNormal(const fs::path& p) {
  fs::path out;
  for (const auto& part : p) {
    if (part.empty() || part == ".") continue;
    if (part == "..") {
      if (!out.empty() && out.filename() != "..") out = out.parent_path();
      else out /= part;
    } else {
      out /= part;
    }
  }
  return out;
}

bool isWithinDirectory(const std::string& base, const std::string& path) {
  std::error_code ec;
  fs::path b = fs::weakly_canonical(base, ec);
  if (ec || !fs::is_directory(b, ec)) return false;
  fs::path target = fs::weakly_canonical(path, ec);
  if (ec) {
    // 目标尚不存在时回退到词法规范化。
    b = lexNormal(fs::absolute(base));
    target = lexNormal(fs::absolute(path));
  }
  auto bc = b.begin();
  auto tc = target.begin();
  for (; bc != b.end(); ++bc, ++tc) {
    if (tc == target.end() || *bc != *tc) return false;
  }
  return true;
}

bool readAll(const std::string& path, std::string* out, std::string* error) {
  std::ifstream f(path, std::ios::binary);
  if (!f) {
    if (error) *error = "cannot open " + path;
    return false;
  }
  std::ostringstream ss;
  ss << f.rdbuf();
  *out = ss.str();
  if (f.bad()) {
    if (error) *error = "read failure on " + path;
    return false;
  }
  return true;
}

bool atomicWrite(const std::string& path, const std::string& content,
                 std::string* error) {
  fs::path target(path);
  fs::path tmp = target;
  tmp += ".tmp-" + std::to_string(::getpid());
  {
    std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
    if (!f) {
      if (error) *error = "cannot create " + tmp.string();
      return false;
    }
    f.write(content.data(), static_cast<std::streamsize>(content.size()));
    if (!f) {
      if (error) *error = "write failure on " + tmp.string();
      return false;
    }
  }
  std::error_code ec;
  fs::rename(tmp, target, ec);
  if (ec) {
    if (error) *error = "rename failed: " + ec.message();
    fs::remove(tmp, ec);
    return false;
  }
  return true;
}

bool exists(const std::string& path) {
  std::error_code ec;
  return fs::exists(path, ec);
}

bool removeFile(const std::string& path, std::string* error) {
  std::error_code ec;
  fs::remove(path, ec);
  if (ec) {
    if (error) *error = ec.message();
    return false;
  }
  return true;
}

uint64_t fnvFile(const std::string& path, size_t chunkBytes) {
  FnvHash h;
  std::ifstream f(path, std::ios::binary);
  if (!f) return 0;
  std::vector<char> buf(chunkBytes);
  while (f) {
    f.read(buf.data(), static_cast<std::streamsize>(buf.size()));
    std::streamsize n = f.gcount();
    if (n > 0) h.update(buf.data(), static_cast<size_t>(n));
  }
  return h.value();
}

}  // namespace media::fsutil

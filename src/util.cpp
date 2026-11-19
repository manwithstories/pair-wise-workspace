#include "util.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <sstream>

namespace mediaqc {

std::string json_escape(std::string_view in) {
  std::string out;
  out.reserve(in.size() + 8);
  for (unsigned char c : in) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\b': out += "\\b"; break;
      case '\f': out += "\\f"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof buf, "\\u%04x", c);
          out += buf;
        } else {
          out += static_cast<char>(c);
        }
    }
  }
  return out;
}

void JsonWriter::comma() {
  if (after_key_) {
    after_key_ = false;
    return;
  }
  if (!first_in_container_) out_ += ',';
  first_in_container_ = false;
}

JsonWriter& JsonWriter::begin_object() {
  comma();
  out_ += '{';
  stack_.push_back('{');
  first_in_container_ = true;
  return *this;
}

JsonWriter& JsonWriter::end_object() {
  out_ += '}';
  stack_.pop_back();
  first_in_container_ = false;
  return *this;
}

JsonWriter& JsonWriter::begin_array() {
  comma();
  out_ += '[';
  stack_.push_back('[');
  first_in_container_ = true;
  return *this;
}

JsonWriter& JsonWriter::end_array() {
  out_ += ']';
  stack_.pop_back();
  first_in_container_ = false;
  return *this;
}

JsonWriter& JsonWriter::key(std::string_view k) {
  comma();
  out_ += '"';
  out_ += json_escape(k);
  out_ += "\":";
  after_key_ = true;
  return *this;
}

JsonWriter& JsonWriter::value(std::string_view v) {
  comma();
  out_ += '"';
  out_ += json_escape(v);
  out_ += '"';
  return *this;
}

JsonWriter& JsonWriter::value(int64_t v) {
  comma();
  out_ += std::to_string(v);
  return *this;
}

JsonWriter& JsonWriter::value(uint64_t v) {
  comma();
  out_ += std::to_string(v);
  return *this;
}

JsonWriter& JsonWriter::value(bool v) {
  comma();
  out_ += v ? "true" : "false";
  return *this;
}

JsonWriter& JsonWriter::value(double v) {
  comma();
  char buf[32];
  std::snprintf(buf, sizeof buf, "%.6f", v);
  out_ += buf;
  return *this;
}

uint64_t fnv1a64(std::string_view data) {
  uint64_t h = 1469598103934665603ULL;
  for (unsigned char c : data) {
    h ^= c;
    h *= 1099511628211ULL;
  }
  return h;
}

int64_t file_size_bytes(const std::filesystem::path& path) {
  std::error_code ec;
  const auto n = std::filesystem::file_size(path, ec);
  if (ec) return -1;
  return static_cast<int64_t>(n);
}

bool read_file(const std::filesystem::path& path, std::string& out, int64_t max_bytes) {
  std::ifstream in(path, std::ios::binary);
  if (!in) return false;
  in.seekg(0, std::ios::end);
  const std::streamoff size = in.tellg();
  if (size < 0 || size > max_bytes) return false;
  in.seekg(0, std::ios::beg);
  out.resize(static_cast<size_t>(size));
  if (size > 0 && !in.read(out.data(), size)) return false;
  return true;
}

bool write_file_atomic(const std::filesystem::path& path, std::string_view data,
                       std::string& err) {
  std::filesystem::path tmp = path;
  tmp += ".tmp";
  {
    std::ofstream out(tmp, std::ios::binary | std::ios::trunc);
    if (!out) {
      err = "cannot open " + tmp.string() + ": " + std::strerror(errno);
      return false;
    }
    out.write(data.data(), static_cast<std::streamsize>(data.size()));
    out.flush();
    if (!out) {
      err = "write failed for " + tmp.string() + ": " + std::strerror(errno);
      std::filesystem::remove(tmp);
      return false;
    }
  }
  // fsync the file before rename so a crash cannot leave a half-written
  // checkpoint under the final name.
  const int fd = ::open(tmp.c_str(), O_RDONLY);
  if (fd >= 0) {
    ::fsync(fd);
    ::close(fd);
  }
  std::error_code ec;
  std::filesystem::rename(tmp, path, ec);
  if (ec) {
    err = "rename " + tmp.string() + " -> " + path.string() + ": " + ec.message();
    std::filesystem::remove(tmp);
    return false;
  }
  return true;
}

bool canonicalize(const std::filesystem::path& path, std::filesystem::path& out) {
  std::error_code ec;
  out = std::filesystem::weakly_canonical(path, ec);
  return !ec;
}

bool path_is_within(const std::filesystem::path& child,
                    const std::filesystem::path& root) {
  if (root.empty()) return true;  // no root configured: caller opted out
  std::filesystem::path c, r;
  if (!canonicalize(child, c) || !canonicalize(root, r)) return false;
  auto ci = c.begin();
  for (auto ri = r.begin(); ri != r.end(); ++ri, ++ci) {
    if (ri->empty()) continue;
    if (ci == c.end() || *ci != *ri) return false;
  }
  return true;
}

int64_t epoch_seconds_now() {
  using namespace std::chrono;
  return duration_cast<seconds>(system_clock::now().time_since_epoch()).count();
}

std::string iso8601_utc(int64_t epoch_seconds) {
  std::time_t t = static_cast<std::time_t>(epoch_seconds);
  std::tm tm{};
  gmtime_r(&t, &tm);
  char buf[32];
  std::strftime(buf, sizeof buf, "%Y-%m-%dT%H:%M:%SZ", &tm);
  return buf;
}

std::string hex64(uint64_t v) {
  char buf[17];
  std::snprintf(buf, sizeof buf, "%016llx", static_cast<unsigned long long>(v));
  return buf;
}

}  // namespace mediaqc

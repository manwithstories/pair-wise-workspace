#include "file_io.hpp"

#include <cerrno>
#include <cstdio>
#include <fstream>
#include <system_error>

#include "hash.hpp"
#include "sha256.hpp"

namespace media_restore {
namespace {

Diagnostic io_error(const std::filesystem::path& path, const std::string& message,
                    ErrorCode code = ErrorCode::io) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.message = message;
  diagnostic.location.file = path.string();
  return diagnostic;
}

constexpr size_t kChunk = 1 << 20;

}  // namespace

bool read_file(const std::filesystem::path& path, size_t max_bytes,
               std::string* out, Diagnostic* error) {
  std::error_code ec;
  const auto size = std::filesystem::file_size(path, ec);
  if (ec) {
    if (error) *error = io_error(path, "cannot stat file: " + ec.message());
    return false;
  }
  if (size > max_bytes) {
    if (error) {
      *error = io_error(path,
                        "file is " + std::to_string(size) +
                            " bytes, limit is " + std::to_string(max_bytes),
                        ErrorCode::limit_exceeded);
    }
    return false;
  }
  std::ifstream stream(path, std::ios::binary);
  if (!stream) {
    if (error) *error = io_error(path, "cannot open file for reading");
    return false;
  }
  out->resize(static_cast<size_t>(size));
  if (size > 0) {
    stream.read(out->data(), static_cast<std::streamsize>(size));
    if (!stream) {
      if (error) *error = io_error(path, "short read");
      return false;
    }
  }
  return true;
}

bool write_file_atomic(const std::filesystem::path& path, std::string_view data,
                       Diagnostic* error) {
  std::error_code ec;
  if (path.has_parent_path()) {
    std::filesystem::create_directories(path.parent_path(), ec);
    if (ec) {
      if (error) *error = io_error(path, "cannot create parent directory: " + ec.message());
      return false;
    }
  }
  const std::filesystem::path temp = path.string() + ".tmp";
  {
    std::ofstream stream(temp, std::ios::binary | std::ios::trunc);
    if (!stream) {
      if (error) *error = io_error(temp, "cannot open temporary file");
      return false;
    }
    stream.write(data.data(), static_cast<std::streamsize>(data.size()));
    stream.flush();
    if (!stream) {
      if (error) *error = io_error(temp, "write failed");
      return false;
    }
  }
  std::filesystem::rename(temp, path, ec);
  if (ec) {
    std::filesystem::remove(temp, ec);
    if (error) *error = io_error(path, "atomic rename failed");
    return false;
  }
  return true;
}

bool write_text_file(const std::filesystem::path& path, std::string_view data,
                     Diagnostic* error) {
  return write_file_atomic(path, data, error);
}

bool ensure_directory(const std::filesystem::path& path, Diagnostic* error) {
  std::error_code ec;
  std::filesystem::create_directories(path, ec);
  if (ec) {
    if (error) *error = io_error(path, "cannot create directory: " + ec.message());
    return false;
  }
  if (!std::filesystem::is_directory(path, ec)) {
    if (error) *error = io_error(path, "path is not a directory");
    return false;
  }
  return true;
}

bool hash_file(const std::filesystem::path& path, std::string* hex,
               Diagnostic* error) {
  std::ifstream stream(path, std::ios::binary);
  if (!stream) {
    if (error) *error = io_error(path, "cannot open file for hashing");
    return false;
  }
  StableHash hash;
  std::vector<char> buffer(kChunk);
  while (stream) {
    stream.read(buffer.data(), static_cast<std::streamsize>(buffer.size()));
    const auto read = stream.gcount();
    if (read > 0) hash.update(buffer.data(), static_cast<size_t>(read));
  }
  if (!stream.eof()) {
    if (error) *error = io_error(path, "read failed while hashing");
    return false;
  }
  *hex = hash.hex();
  return true;
}

bool sha256_file(const std::filesystem::path& path, std::string* hex,
                 Diagnostic* error) {
  std::ifstream stream(path, std::ios::binary);
  if (!stream) {
    if (error) *error = io_error(path, "cannot open file for hashing");
    return false;
  }
  Sha256 hash;
  std::vector<char> buffer(kChunk);
  while (stream) {
    stream.read(buffer.data(), static_cast<std::streamsize>(buffer.size()));
    const auto read = stream.gcount();
    if (read > 0) hash.update(buffer.data(), static_cast<size_t>(read));
  }
  if (!stream.eof()) {
    if (error) *error = io_error(path, "read failed while hashing");
    return false;
  }
  *hex = hash.hex();
  return true;
}

bool file_size(const std::filesystem::path& path, int64_t* size,
               Diagnostic* error) {
  std::error_code ec;
  const auto value = std::filesystem::file_size(path, ec);
  if (ec) {
    if (error) *error = io_error(path, "cannot stat file: " + ec.message());
    return false;
  }
  if (value > static_cast<uintmax_t>(INT64_MAX)) {
    if (error) *error = io_error(path, "file size overflows int64", ErrorCode::limit_exceeded);
    return false;
  }
  *size = static_cast<int64_t>(value);
  return true;
}

bool resolve_archive_path(const std::filesystem::path& manifest_path,
                          std::string_view relative,
                          std::filesystem::path* out,
                          Diagnostic* error) {
  const std::filesystem::path candidate{std::string(relative)};
  if (candidate.is_absolute()) {
    if (error) {
      *error = io_error(candidate, "archive paths must be relative");
    }
    return false;
  }
  for (const auto& part : candidate) {
    if (part == "..") {
      if (error) *error = io_error(candidate, "archive paths must not contain '..'");
      return false;
    }
  }
  *out = manifest_path.parent_path() / candidate;
  return true;
}

bool read_prefix(const std::filesystem::path& path, int64_t offset,
                 size_t length, std::string* out, Diagnostic* error) {
  std::ifstream stream(path, std::ios::binary);
  if (!stream) {
    if (error) *error = io_error(path, "cannot open file");
    return false;
  }
  if (offset < 0) {
    if (error) *error = io_error(path, "negative read offset");
    return false;
  }
  stream.seekg(offset, std::ios::beg);
  if (!stream) {
    if (error) *error = io_error(path, "seek failed");
    return false;
  }
  out->resize(length);
  stream.read(out->data(), static_cast<std::streamsize>(length));
  out->resize(static_cast<size_t>(stream.gcount()));
  if (stream.bad()) {
    if (error) *error = io_error(path, "read failed");
    return false;
  }
  return true;
}

}  // namespace media_restore

#include "image_io.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cctype>
#include <fstream>
#include <limits>
#include <system_error>

namespace lutgrade {
namespace {

std::atomic<std::uint64_t> temporary_counter{0};

std::string netpbm_error(const std::filesystem::path& path, const std::string& message) {
  return path.string() + ": " + message;
}

void ensure_parent_directory(const std::filesystem::path& path) {
  const std::filesystem::path parent = path.parent_path();
  if (parent.empty()) {
    return;
  }
  std::error_code error;
  std::filesystem::create_directories(parent, error);
  if (error) {
    throw ImageIoError(netpbm_error(parent, "cannot create output directory: " + error.message()));
  }
}

std::filesystem::path temporary_path(const std::filesystem::path& final_path) {
  const std::uint64_t serial = temporary_counter.fetch_add(1, std::memory_order_relaxed);
  const auto ticks = std::chrono::steady_clock::now().time_since_epoch().count();
  return final_path.parent_path() /
         ("." + final_path.filename().string() + ".tmp." + std::to_string(ticks) + "." +
          std::to_string(serial));
}

void remove_silently(const std::filesystem::path& path) {
  std::error_code ignored;
  std::filesystem::remove(path, ignored);
}

void commit_temp(const std::filesystem::path& temporary, const std::filesystem::path& final_path) {
  std::error_code error;
  std::filesystem::rename(temporary, final_path, error);
  if (error) {
    remove_silently(temporary);
    throw ImageIoError(netpbm_error(final_path, "cannot finalize output: " + error.message()));
  }
}

void write_exact(std::ofstream& output, const char* data, std::size_t size,
                 const std::filesystem::path& path) {
  output.write(data, static_cast<std::streamsize>(size));
  if (!output) {
    throw ImageIoError(netpbm_error(path, "write failed"));
  }
}

struct EncodedNetpbm {
  std::string header;
  std::vector<unsigned char> raster;
};

EncodedNetpbm encode_netpbm(const std::filesystem::path& path, const FloatImage& image) {
  const std::uint64_t expected =
      static_cast<std::uint64_t>(image.width) * image.height * image.channels;
  if (image.width == 0 || image.height == 0 || (image.channels != 1 && image.channels != 3) ||
      image.pixels.size() != expected) {
    throw ImageIoError(netpbm_error(path, "pixel buffer dimensions do not match image metadata"));
  }

  const bool grayscale = path.extension() == ".pgm" || image.channels == 1;
  EncodedNetpbm result;
  result.header = std::string(grayscale ? "P5" : "P6") + "\n" +
                  std::to_string(image.width) + " " + std::to_string(image.height) + "\n65535\n";
  result.raster.reserve(static_cast<std::size_t>(image.width) * image.height *
                        (grayscale ? 2 : 6));
  for (std::size_t pixel = 0; pixel < static_cast<std::size_t>(image.width) * image.height;
       ++pixel) {
    for (std::uint8_t channel = 0; channel < (grayscale ? 1 : 3); ++channel) {
      double value = 0.0;
      if (image.channels == 1) {
        value = image.pixels[pixel];
      } else if (grayscale) {
        value = (image.pixels[pixel * 3] + image.pixels[pixel * 3 + 1] +
                 image.pixels[pixel * 3 + 2]) /
                3.0;
      } else {
        value = image.pixels[pixel * 3 + channel];
      }
      const std::uint16_t sample = static_cast<std::uint16_t>(
          std::llround(std::clamp(value, 0.0, 1.0) * 65535.0));
      result.raster.push_back(static_cast<unsigned char>(sample >> 8U));
      result.raster.push_back(static_cast<unsigned char>(sample & 0xFFU));
    }
  }
  return result;
}

void write_temp_file(const std::filesystem::path& temporary,
                     const std::filesystem::path& final_path,
                     const std::string& header, const std::vector<unsigned char>& raster) {
  std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
  if (!output) {
    throw ImageIoError(netpbm_error(final_path, "cannot create temporary output"));
  }
  write_exact(output, header.data(), header.size(), final_path);
  write_exact(output, reinterpret_cast<const char*>(raster.data()), raster.size(), final_path);
  output.close();
  if (!output) {
    throw ImageIoError(netpbm_error(final_path, "write failed while closing output"));
  }
}

void write_temp_text_file(const std::filesystem::path& temporary,
                          const std::filesystem::path& final_path, const std::string& text) {
  std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
  if (!output) {
    throw ImageIoError(netpbm_error(final_path, "cannot create temporary output"));
  }
  write_exact(output, text.data(), text.size(), final_path);
  output.close();
  if (!output) {
    throw ImageIoError(netpbm_error(final_path, "write failed while closing output"));
  }
}

struct Backup {
  std::filesystem::path original;
  std::filesystem::path saved;
  bool active = false;
};

Backup back_up_existing(const std::filesystem::path& path) {
  Backup backup;
  backup.original = path;
  std::error_code error;
  if (!std::filesystem::exists(path, error)) {
    if (error) {
      throw ImageIoError(netpbm_error(path, "cannot inspect existing output: " + error.message()));
    }
    return backup;
  }
  if (std::filesystem::is_directory(path, error)) {
    throw ImageIoError(netpbm_error(path, "output path is a directory"));
  }
  backup.saved = temporary_path(path);
  std::filesystem::rename(path, backup.saved, error);
  if (error) {
    throw ImageIoError(netpbm_error(path, "cannot back up existing output: " + error.message()));
  }
  backup.active = true;
  return backup;
}

void remove_backup(const Backup& backup) {
  if (backup.active) {
    remove_silently(backup.saved);
  }
}

void restore_backup(Backup& backup) {
  if (!backup.active) {
    return;
  }
  remove_silently(backup.original);
  std::error_code error;
  std::filesystem::rename(backup.saved, backup.original, error);
  backup.active = false;
}

bool same_lexical_path(const std::filesystem::path& first,
                       const std::filesystem::path& second) {
  std::error_code error;
  const auto first_absolute = std::filesystem::absolute(first, error).lexically_normal();
  if (error) return false;
  const auto second_absolute = std::filesystem::absolute(second, error).lexically_normal();
  return !error && first_absolute == second_absolute;
}

std::string read_token(std::ifstream& input) {
  char ch = '\0';
  while (input.get(ch)) {
    if (ch == '#') {
      while (input.get(ch) && ch != '\n' && ch != '\r') {
      }
      continue;
    }
    if (!std::isspace(static_cast<unsigned char>(ch))) {
      break;
    }
  }
  if (!input) {
    return {};
  }

  std::string token;
  token.push_back(ch);
  while (input.get(ch)) {
    if (std::isspace(static_cast<unsigned char>(ch)) || ch == '#') {
      input.unget();
      break;
    }
    token.push_back(ch);
  }
  return token;
}

std::uint32_t parse_positive_token(const std::string& token, const std::filesystem::path& path,
                                   const char* label, std::uint32_t maximum) {
  if (token.empty() || token.size() > 10 ||
      !std::all_of(token.begin(), token.end(), [](unsigned char ch) { return std::isdigit(ch); })) {
    throw ImageIoError(netpbm_error(path, std::string("invalid ") + label));
  }
  const std::uint64_t value = std::stoull(token);
  if (value == 0 || value > maximum) {
    throw ImageIoError(netpbm_error(path, std::string(label) + " out of range"));
  }
  return static_cast<std::uint32_t>(value);
}

void consume_header_separator(std::ifstream& input, const std::filesystem::path& path) {
  char separator = '\0';
  if (!input.get(separator) || !std::isspace(static_cast<unsigned char>(separator))) {
    throw ImageIoError(netpbm_error(path, "missing whitespace after max value"));
  }
  if (separator == '\r' && input.peek() == '\n') {
    input.get(separator);
  }
}

}  // namespace

FloatImage read_image(const std::filesystem::path& path) {
  std::ifstream input(path, std::ios::binary);
  if (!input) {
    throw ImageIoError(netpbm_error(path, "cannot open input image"));
  }

  const std::string magic = read_token(input);
  if (magic != "P5" && magic != "P6") {
    throw ImageIoError(netpbm_error(path, "expected binary P5/P6 image"));
  }
  const std::uint32_t width = parse_positive_token(read_token(input), path, "width", 65535);
  const std::uint32_t height = parse_positive_token(read_token(input), path, "height", 65535);
  const std::uint32_t max_value = parse_positive_token(read_token(input), path, "max value", 65535);
  consume_header_separator(input, path);

  const std::uint8_t channels = magic == "P6" ? 3 : 1;
  const std::uint8_t bytes_per_sample = max_value < 256 ? 1 : 2;
  const std::uint64_t pixel_count = static_cast<std::uint64_t>(width) * height;
  const std::uint64_t byte_count = pixel_count * channels * bytes_per_sample;
  if (byte_count > static_cast<std::uint64_t>(std::numeric_limits<std::size_t>::max())) {
    throw ImageIoError(netpbm_error(path, "image is too large"));
  }

  std::vector<unsigned char> bytes(static_cast<std::size_t>(byte_count));
  input.read(reinterpret_cast<char*>(bytes.data()), static_cast<std::streamsize>(bytes.size()));
  if (static_cast<std::size_t>(input.gcount()) != bytes.size()) {
    throw ImageIoError(netpbm_error(path, "truncated pixel data"));
  }

  FloatImage image;
  image.width = width;
  image.height = height;
  image.channels = channels;
  image.pixels.resize(static_cast<std::size_t>(pixel_count) * channels);
  const double scale = 1.0 / static_cast<double>(max_value);
  for (std::size_t i = 0; i < image.pixels.size(); ++i) {
    const std::uint32_t sample =
        bytes_per_sample == 1
            ? static_cast<std::uint32_t>(bytes[i])
            : (static_cast<std::uint32_t>(bytes[i * 2]) << 8U) |
                  static_cast<std::uint32_t>(bytes[i * 2 + 1]);
    image.pixels[i] = static_cast<float>(static_cast<double>(sample) * scale);
  }
  return image;
}

FloatImage as_rgb(const FloatImage& image) {
  if (image.channels == 3) {
    return image;
  }
  if (image.channels != 1) {
    throw ImageIoError("cannot convert image with unsupported channel count to RGB");
  }
  FloatImage result;
  result.width = image.width;
  result.height = image.height;
  result.channels = 3;
  result.pixels.resize(static_cast<std::size_t>(image.width) * image.height * 3);
  for (std::size_t pixel = 0; pixel < image.pixels.size(); ++pixel) {
    result.pixels[pixel * 3] = image.pixels[pixel];
    result.pixels[pixel * 3 + 1] = image.pixels[pixel];
    result.pixels[pixel * 3 + 2] = image.pixels[pixel];
  }
  return result;
}

void check_output_writable(const std::filesystem::path& path) {
  ensure_parent_directory(path);
  std::error_code error;
  if (std::filesystem::is_directory(path, error)) {
    throw ImageIoError(netpbm_error(path, "output path is a directory"));
  }
  const std::filesystem::path probe = temporary_path(path);
  {
    std::ofstream output(probe, std::ios::binary | std::ios::trunc);
    if (!output) {
      throw ImageIoError(netpbm_error(path, "output is not writable"));
    }
  }
  std::filesystem::remove(probe, error);
  if (error) {
    throw ImageIoError(netpbm_error(path, "cannot clean output probe: " + error.message()));
  }
}

void write_image_atomic(const std::filesystem::path& path, const FloatImage& image) {
  const EncodedNetpbm encoded = encode_netpbm(path, image);
  ensure_parent_directory(path);
  const std::filesystem::path temporary = temporary_path(path);
  try {
    write_temp_file(temporary, path, encoded.header, encoded.raster);
    commit_temp(temporary, path);
  } catch (...) {
    remove_silently(temporary);
    throw;
  }
}

void write_text_atomic(const std::filesystem::path& path, const std::string& text) {
  ensure_parent_directory(path);
  const std::filesystem::path temporary = temporary_path(path);
  try {
    std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
    if (!output) {
      throw ImageIoError(netpbm_error(path, "cannot create temporary output"));
    }
    write_exact(output, text.data(), text.size(), path);
    output.close();
    if (!output) {
      throw ImageIoError(netpbm_error(path, "write failed while closing output"));
    }
    commit_temp(temporary, path);
  } catch (...) {
    remove_silently(temporary);
    throw;
  }
}

void write_image_and_text_atomic(const std::filesystem::path& image_path,
                                 const FloatImage& image,
                                 const std::filesystem::path& text_path,
                                 const std::string& text) {
  if (same_lexical_path(image_path, text_path)) {
    throw ImageIoError("image and report must use different output paths");
  }
  const EncodedNetpbm encoded = encode_netpbm(image_path, image);
  ensure_parent_directory(image_path);
  ensure_parent_directory(text_path);

  const std::filesystem::path image_temp = temporary_path(image_path);
  const std::filesystem::path text_temp = temporary_path(text_path);
  Backup image_backup;
  Backup text_backup;
  bool image_committed = false;
  bool text_committed = false;
  try {
    write_temp_file(image_temp, image_path, encoded.header, encoded.raster);
    write_temp_text_file(text_temp, text_path, text);
    image_backup = back_up_existing(image_path);
    text_backup = back_up_existing(text_path);
    commit_temp(image_temp, image_path);
    image_committed = true;
    commit_temp(text_temp, text_path);
    text_committed = true;
    remove_backup(image_backup);
    remove_backup(text_backup);
  } catch (...) {
    if (text_committed) remove_silently(text_path);
    if (image_committed) remove_silently(image_path);
    restore_backup(text_backup);
    restore_backup(image_backup);
    remove_silently(image_temp);
    remove_silently(text_temp);
    throw;
  }
}

}  // namespace lutgrade


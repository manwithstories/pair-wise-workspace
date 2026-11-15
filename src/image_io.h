#pragma once

#include <cstdint>
#include <filesystem>
#include <stdexcept>
#include <string>
#include <vector>

namespace lutgrade {

struct FloatImage {
  std::uint32_t width = 0;
  std::uint32_t height = 0;
  std::uint8_t channels = 0;
  std::vector<float> pixels;

  float& at(std::uint32_t x, std::uint32_t y, std::uint8_t channel) {
    return pixels[(static_cast<std::size_t>(y) * width + x) * channels + channel];
  }

  float at(std::uint32_t x, std::uint32_t y, std::uint8_t channel) const {
    return pixels[(static_cast<std::size_t>(y) * width + x) * channels + channel];
  }
};

class ImageIoError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

FloatImage read_image(const std::filesystem::path& path);
FloatImage as_rgb(const FloatImage& image);
void check_output_writable(const std::filesystem::path& path);
void write_image_atomic(const std::filesystem::path& path, const FloatImage& image);
void write_text_atomic(const std::filesystem::path& path, const std::string& text);
void write_image_and_text_atomic(const std::filesystem::path& image_path,
                                 const FloatImage& image,
                                 const std::filesystem::path& text_path,
                                 const std::string& text);

}  // namespace lutgrade


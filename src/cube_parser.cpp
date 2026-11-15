#include "cube_parser.h"

#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <limits>
#include <sstream>

namespace lutgrade {
namespace {

std::string trim(const std::string& input) {
  const std::string whitespace = " \t\r\n";
  const std::size_t first = input.find_first_not_of(whitespace);
  if (first == std::string::npos) {
    return {};
  }
  const std::size_t last = input.find_last_not_of(whitespace);
  return input.substr(first, last - first + 1);
}

std::vector<std::string> split_words(const std::string& line) {
  std::istringstream stream(line);
  std::vector<std::string> words;
  std::string word;
  while (stream >> word) {
    words.push_back(word);
  }
  return words;
}

double parse_number(const std::string& token, std::size_t line_number) {
  errno = 0;
  char* end = nullptr;
  const double value = std::strtod(token.c_str(), &end);
  if (end == token.c_str() || *end != '\0' || !std::isfinite(value)) {
    throw CubeError("invalid numeric value at line " + std::to_string(line_number) + ": " + token);
  }
  if (errno == ERANGE && std::abs(value) == 0.0) {
    throw CubeError("numeric value underflow at line " + std::to_string(line_number) + ": " + token);
  }
  return value;
}

int parse_grid_size(const std::vector<std::string>& words, const std::string& directive,
                    std::size_t line_number, int maximum) {
  if (words.size() != 2) {
    throw CubeError(directive + " requires one value at line " + std::to_string(line_number));
  }
  const double parsed = parse_number(words[1], line_number);
  if (parsed < 1.0 || parsed > static_cast<double>(maximum) || std::floor(parsed) != parsed) {
    throw CubeError("grid size out of range at line " + std::to_string(line_number));
  }
  return static_cast<int>(parsed);
}

Vec3 parse_vec3(const std::vector<std::string>& words, std::size_t line_number,
                std::size_t offset = 0) {
  if (words.size() != offset + 3) {
    throw CubeError("expected three values at line " + std::to_string(line_number));
  }
  return {parse_number(words[offset], line_number), parse_number(words[offset + 1], line_number),
          parse_number(words[offset + 2], line_number)};
}

std::size_t expected_3d_entries(int size) {
  const std::size_t n = static_cast<std::size_t>(size);
  if (n > std::numeric_limits<std::size_t>::max() / n / n) {
    throw CubeError("LUT_3D_SIZE is too large");
  }
  return n * n * n;
}

}  // namespace

CubeLut parse_cube_file(const std::filesystem::path& path) {
  std::ifstream input(path);
  if (!input) {
    throw CubeError("cannot open LUT file: " + path.string());
  }

  enum class Stage { None, Shaper, ShaperThen3d, Table3d };
  CubeLut lut;
  Stage stage = Stage::None;
  bool saw_domain_min = false;
  bool saw_domain_max = false;
  bool saw_shaper_size = false;
  bool saw_3d_size = false;
  std::string raw_line;
  std::size_t line_number = 0;

  while (std::getline(input, raw_line)) {
    ++line_number;
    const std::size_t comment = raw_line.find('#');
    if (comment != std::string::npos) {
      raw_line.erase(comment);
    }
    const std::string line = trim(raw_line);
    if (line.empty()) {
      continue;
    }
    const std::vector<std::string> words = split_words(line);
    const std::string& first = words.front();

    if (first == "LUT_1D_SIZE") {
      if (saw_shaper_size || saw_3d_size || stage == Stage::Table3d) {
        throw CubeError("unexpected LUT_1D_SIZE at line " + std::to_string(line_number));
      }
      saw_shaper_size = true;
      lut.shaper_size = parse_grid_size(words, first, line_number, 65536);
      stage = Stage::Shaper;
      continue;
    }
    if (first == "LUT_3D_SIZE") {
      if (saw_3d_size) {
        throw CubeError("duplicate LUT_3D_SIZE at line " + std::to_string(line_number));
      }
      saw_3d_size = true;
      lut.size_3d = parse_grid_size(words, first, line_number, 256);
      stage = (saw_shaper_size && lut.shaper.size() < static_cast<std::size_t>(lut.shaper_size))
                  ? Stage::ShaperThen3d
                  : Stage::Table3d;
      continue;
    }
    if (first == "DOMAIN_MIN" || first == "DOMAIN_MAX") {
      const Vec3 value = parse_vec3(words, line_number, 1);
      if (first == "DOMAIN_MIN") {
        if (saw_domain_min) {
          throw CubeError("duplicate DOMAIN_MIN at line " + std::to_string(line_number));
        }
        saw_domain_min = true;
        lut.domain_min = value;
      } else {
        if (saw_domain_max) {
          throw CubeError("duplicate DOMAIN_MAX at line " + std::to_string(line_number));
        }
        saw_domain_max = true;
        lut.domain_max = value;
      }
      continue;
    }
    if (first == "TITLE") {
      continue;
    }

    if (stage == Stage::None) {
      throw CubeError("LUT entry before grid size at line " + std::to_string(line_number));
    }
    const Vec3 entry = parse_vec3(words, line_number);
    if (stage == Stage::Shaper || stage == Stage::ShaperThen3d) {
      if (stage == Stage::ShaperThen3d &&
          lut.shaper.size() == static_cast<std::size_t>(lut.shaper_size)) {
        const std::size_t expected = expected_3d_entries(lut.size_3d);
        if (lut.table.size() >= expected) {
          throw CubeError("too many 3D LUT entries at line " + std::to_string(line_number));
        }
        lut.table.push_back(entry);
        continue;
      }
      if (lut.shaper.size() >= static_cast<std::size_t>(lut.shaper_size)) {
        throw CubeError("too many 1D shaper entries at line " + std::to_string(line_number));
      }
      lut.shaper.push_back(entry);
    } else {
      const std::size_t expected = expected_3d_entries(lut.size_3d);
      if (lut.table.size() >= expected) {
        throw CubeError("too many 3D LUT entries at line " + std::to_string(line_number));
      }
      lut.table.push_back(entry);
    }
  }

  if (!saw_shaper_size && !saw_3d_size) {
    throw CubeError("missing LUT_1D_SIZE or LUT_3D_SIZE");
  }
  if (saw_shaper_size && lut.shaper.size() != static_cast<std::size_t>(lut.shaper_size)) {
    throw CubeError("1D shaper entry count does not match LUT_1D_SIZE");
  }
  if (saw_3d_size && lut.table.size() != expected_3d_entries(lut.size_3d)) {
    throw CubeError("3D LUT entry count does not match LUT_3D_SIZE");
  }
  if (!(lut.domain_max.r > lut.domain_min.r) || !(lut.domain_max.g > lut.domain_min.g) ||
      !(lut.domain_max.b > lut.domain_min.b)) {
    throw CubeError("DOMAIN_MAX must be greater than DOMAIN_MIN on every channel");
  }
  return lut;
}

}  // namespace lutgrade


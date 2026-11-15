#pragma once
#include <cstddef>
#include <map>
#include <string>
#include <string_view>
#include <vector>

namespace smx {
struct CueStyle {
  std::string name;
  std::map<std::string, std::string> attributes;
  std::size_t line = 0;
};

struct Cue {
  std::string id;
  std::string text;
  std::string style = "default";
  std::string segment;
  std::size_t line = 0;
  std::size_t invalid_utf8 = 0;
};

struct CueParseError {
  std::size_t line = 0;
  std::string message;
};

struct CueDocument {
  std::vector<CueStyle> styles;
  std::vector<Cue> cues;
  std::vector<CueParseError> errors;
  std::size_t invalid_utf8 = 0;
};

CueDocument parse_cues(std::string_view source);
CueDocument parse_cue_file(const std::string& path, std::string& io_error);
}  // namespace smx

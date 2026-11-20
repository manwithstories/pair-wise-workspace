#pragma once

// Caption parsing: SRT and WebVTT onto the unified millisecond axis.
// Raw time strings and byte offsets are preserved; malformed times,
// overlaps, truncated lines and a missing trailing newline are reported
// as recoverable anomalies while scanning continues with later cues.

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "relay/error_model.hpp"

namespace relay {

enum class CaptionFormat { Srt, WebVtt };

struct Cue {
  int64_t start_ms = 0;
  int64_t end_ms = 0;
  std::string raw_start;   // timestamp text as written in the file
  std::string raw_end;
  uint64_t byte_offset = 0;  // offset of the cue block in the source
  uint32_t index = 0;        // 1-based cue ordinal in parse order
};

struct CaptionParseResult {
  CaptionFormat format = CaptionFormat::Srt;
  std::vector<Cue> cues;
  std::vector<AnalysisError> anomalies;
};

CaptionParseResult parse_srt(std::string_view text);
CaptionParseResult parse_webvtt(std::string_view text);
// Sniff the format (WEBVTT magic) and dispatch.
CaptionParseResult parse_captions(std::string_view text);

}  // namespace relay

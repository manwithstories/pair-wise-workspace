#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

#include "media/json.hpp"
#include "media/sha256.hpp"
#include "media/types.hpp"

namespace mediaqc {

// One supplier delivery directory:
//
//   <input>/delivery.json        versioned schema: tracks, codecs, clocks
//   <input>/<track>.frames.jsonl one JSON object per frame, in capture order
//   <input>/<track>.mkv          media payload for that track
//
// The metadata line is the source of truth for frame attributes; the payload
// only supplies bytes.  Offsets reported to operators are the byte offset of
// the start of the offending JSONL line, so they can be reproduced byte for
// byte with `grep -b`.
struct DeliveryTrack {
  TrackDesc desc;
  std::string metadata_file;  // path relative to the delivery directory
  std::string media_file;     // path relative to the delivery directory
  Json extra;                 // unknown track keys, preserved
};

struct Delivery {
  int schema_version = 0;
  std::string producer;
  std::string created;  // free-form ISO timestamp, retained verbatim
  std::vector<DeliveryTrack> tracks;
  Json extra;  // unknown top-level keys, preserved
};

struct DeliveryLoadResult {
  bool ok = false;
  Delivery delivery;
  Error error;
};

// The schema version this build understands.
inline constexpr int kSchemaVersion = 1;

// Loads and validates <dir>/delivery.json.
[[nodiscard]] DeliveryLoadResult load_delivery(const std::string& dir);

// Resolves a relative delivery member path safely: after lexical
// normalisation and symlink resolution it must stay inside the delivery
// directory.  Symlink escapes are rejected.
[[nodiscard]] bool resolve_delivery_path(const std::string& dir,
                                         const std::string& relative,
                                         std::string& absolute, Error& error);

// Fields every frame record must carry regardless of the declared schema.
[[nodiscard]] const std::vector<std::string>& default_required_frame_fields();

// One parsed frame metadata record.
struct FrameMetadata {
  std::uint64_t line = 0;           // 1-based line number in the JSONL file
  std::uint64_t offset = 0;         // byte offset of the line start
  std::uint64_t seq = 0;
  bool has_capture = false;
  std::int64_t capture_ticks = 0;
  bool keyframe = false;
  std::optional<PixelFormatId> pix_fmt;
  int width = 0;
  int height = 0;
  Sha256Digest sha{};
  Json attrs;                       // shooting metadata plus unknown optionals
};

struct FrameParseResult {
  bool ok = false;
  FrameMetadata frame;
  Error error;
};

// Parses and validates one metadata line against `track`.  Unknown fields are
// retained in `frame.attrs`; a missing field that the schema lists as required
// is an error that points at the line's byte offset.
[[nodiscard]] FrameParseResult parse_frame_metadata(std::string_view line,
                                                    std::uint64_t line_no,
                                                    std::uint64_t offset,
                                                    const TrackDesc& track);

// Reads a whole JSONL file and appends parsed records.  A truncated final line
// or invalid JSON yields an error carrying the offending offset.
[[nodiscard]] bool load_frame_metadata(const std::string& path,
                                       const TrackDesc& track,
                                       std::vector<FrameMetadata>& out,
                                       Error& error);

}  // namespace mediaqc

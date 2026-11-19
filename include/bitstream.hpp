#pragma once

#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "error.hpp"
#include "file_io.hpp"
#include "manifest.hpp"

namespace media_restore {

// A top-level coded unit found in a raw bitstream: an Annex B NAL unit for
// H.264/H.265 or an OBU for AV1.  `offset` is the byte position of the unit
// prefix (start code / OBU header), which is what keyframe indexes record.
struct StreamUnit {
  int64_t offset = 0;
  int64_t length = 0;
  int nal_type = -1;
  bool keyframe = false;
};

struct StreamScan {
  Codec codec = Codec::h264;
  int64_t stream_size = 0;
  size_t unit_count = 0;
  std::vector<StreamUnit> units;      // every unit, in file order
  std::vector<StreamUnit> keyframes;  // subset with keyframe == true
};

// Content-based codec sniffing.  Used to reject a file whose bytes do not
// match the codec marker declared in the manifest.
bool sniff_codec(std::string_view prefix, Codec* codec);

// Scans a raw bitstream with bounded memory (chunked for Annex B, seek-based
// for AV1 OBUs).  Enforces Limits::max_index_entries and max_media_bytes.
bool scan_raw_stream(const std::filesystem::path& path, Codec codec,
                     const Limits& limits, StreamScan* out,
                     std::vector<Diagnostic>* diagnostics);

// In-memory variant used by tests and the fixture generator.
bool scan_raw_stream_bytes(std::string_view bytes, Codec codec,
                           const Limits& limits, StreamScan* out,
                           std::vector<Diagnostic>* diagnostics);

// Loads an external keyframe index file:
//   {"format": "media-archive-keyframe-index", "version": 1,
//    "codec": "h264", "entries": [...]}
// Entries from the manifest take precedence when both exist.
bool load_keyframe_index(const std::filesystem::path& path, const Limits& limits,
                         KeyframeIndex* out,
                         std::vector<Diagnostic>* diagnostics);

enum class EntryOrigin { index, scan };

const char* entry_origin_name(EntryOrigin origin);

struct KeyframeCandidate {
  int64_t offset = 0;
  std::optional<int64_t> tick;  // nullopt for values recovered by scanning
  bool keyframe = true;
  EntryOrigin origin = EntryOrigin::index;
};

struct IndexReport {
  size_t index_entries = 0;   // entries supplied by the manifest/index file
  size_t hits = 0;            // supplied entries that match a scan keyframe
  size_t generated = 0;       // scan candidates not present in the index
  size_t scan_keyframes = 0;  // keyframes discovered by scanning
  std::vector<KeyframeCandidate> candidates;
};

// Validates a versioned keyframe index against a scanned bitstream.  Rejects
// negative, out-of-range, duplicate, descending and non-keyframe entries.
// Missing keyframes are filled from the scan and tagged with origin=scan.
bool validate_keyframe_index(const KeyframeIndex& index, const StreamScan& scan,
                             const Limits& limits, IndexReport* report,
                             std::vector<Diagnostic>* diagnostics);

}  // namespace media_restore

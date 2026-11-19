#pragma once

#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "error.hpp"
#include "manifest.hpp"

namespace media_restore {

enum class ColorStrategy {
  preserve,  // keep the source values; conflicts are reported but not applied
  archive,   // write the archive value for any conflicting field
};

struct ArchiveColor {
  std::string primaries = "bt709";
  std::string transfer = "bt709";
  std::string matrix = "bt709";
  std::string range = "tv";
};

bool parse_color_strategy(std::string_view text, ColorStrategy* out);
const char* color_strategy_name(ColorStrategy strategy);

// One resolved field for a segment.  `sources` lists every distinct value
// that was found, in declaration order, so a conflict can be reproduced.
struct ResolvedField {
  std::optional<std::string> value;
  std::string source;                  // segment/track the value came from
  std::vector<std::string> conflicts;  // other values found for this field
  bool missing_from_segment = false;   // inherited from the track consensus
  bool applied_archive = false;        // archive override was written
};

struct SegmentColor {
  std::string track_id;
  std::string segment_id;
  ResolvedField primaries;
  ResolvedField transfer;
  ResolvedField matrix;
  ResolvedField range;
  bool missing_any = false;
  bool conflicted = false;
};

struct ColorReport {
  std::vector<SegmentColor> segments;
  size_t missing_fields = 0;
  size_t conflicts = 0;
};

// Resolves the color metadata for every segment.  Conflicting values are
// never silently overwritten: each segment/field disagreement is a warning
// diagnostic listing every value seen, and the chosen value depends on the
// strategy.  `report.conflicts` counts those segment/field pairs.
bool resolve_color_metadata(const Manifest& manifest, ColorStrategy strategy,
                            const ArchiveColor& archive, ColorReport* report,
                            std::vector<Diagnostic>* diagnostics);

}  // namespace media_restore

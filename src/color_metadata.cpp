#include "color_metadata.hpp"

#include <set>

namespace media_restore {
namespace {

struct FieldView {
  const char* name;
  std::optional<std::string> ColorMetadata::*member;
};

constexpr FieldView kFields[] = {
    {"primaries", &ColorMetadata::primaries},
    {"transfer", &ColorMetadata::transfer},
    {"matrix", &ColorMetadata::matrix},
    {"range", &ColorMetadata::range},
};

std::string archive_value(const ArchiveColor& archive, std::string_view field) {
  if (field == "primaries") return archive.primaries;
  if (field == "transfer") return archive.transfer;
  if (field == "matrix") return archive.matrix;
  return archive.range;
}

Location location_for(const std::string& track, const std::string& segment,
                      const char* field) {
  Location location;
  location.track = track;
  location.segment = segment;
  location.field = field;
  return location;
}

}  // namespace

bool parse_color_strategy(std::string_view text, ColorStrategy* out) {
  if (text == "preserve") {
    *out = ColorStrategy::preserve;
    return true;
  }
  if (text == "archive") {
    *out = ColorStrategy::archive;
    return true;
  }
  return false;
}

const char* color_strategy_name(ColorStrategy strategy) {
  return strategy == ColorStrategy::preserve ? "preserve" : "archive";
}

namespace {

SegmentColor resolve_one(const Track& track, const Segment& segment,
                         ColorStrategy strategy, const ArchiveColor& archive,
                         size_t* missing_fields, size_t* conflicts,
                         std::vector<Diagnostic>* diagnostics) {
  // Track consensus is computed from every segment of the track, so a
  // segment that omits a field can inherit an unambiguous value.
  std::set<std::string> track_values[4];
  std::optional<std::string> track_value[4];
  for (const auto& sibling : track.segments) {
    for (size_t i = 0; i < 4; ++i) {
      const auto& value = sibling.color.*(kFields[i].member);
      if (value) track_values[i].insert(*value);
    }
  }
  for (size_t i = 0; i < 4; ++i) {
    if (track_values[i].size() == 1) track_value[i] = *track_values[i].begin();
  }

  SegmentColor resolved;
  resolved.track_id = track.id;
  resolved.segment_id = segment.id;
  for (size_t i = 0; i < 4; ++i) {
    const auto& field = kFields[i];
    const auto& declared = segment.color.*(field.member);
    ResolvedField output;
    if (declared) {
      output.value = *declared;
      output.source = segment.color.source.empty() ? track.id : segment.color.source;
    } else if (track_value[i]) {
      output.value = *track_value[i];
      output.source = track.id;
      output.missing_from_segment = true;
    }
    if (declared && track_values[i].size() > 1) {
      for (const auto& value : track_values[i]) {
        if (value != *declared) output.conflicts.push_back(value);
      }
    }
    if (!output.conflicts.empty()) {
      resolved.conflicted = true;
      ++*conflicts;
      std::string all;
      for (const auto& value : track_values[i]) {
        if (!all.empty()) all += "/";
        all += value;
      }
      const std::string chosen =
          strategy == ColorStrategy::preserve
              ? output.value.value_or("<none>")
              : archive_value(archive, field.name);
      diagnostics->push_back(Diagnostic{
          .code = ErrorCode::color_conflict,
          .severity = Severity::warning,
          .message = "color " + std::string(field.name) + " conflicts: {" + all +
                     "} (strategy=" + color_strategy_name(strategy) +
                     ", using '" + chosen + "')",
          .location = location_for(track.id, segment.id, field.name)});
      if (strategy == ColorStrategy::archive) {
        output.value = archive_value(archive, field.name);
        output.source = "archive";
        output.applied_archive = true;
      }
    }
    if (!output.value) {
      resolved.missing_any = true;
      ++*missing_fields;
      diagnostics->push_back(Diagnostic{
          .code = ErrorCode::color_conflict,
          .severity = Severity::warning,
          .message = std::string("color ") + field.name +
                     " is missing from both the segment and the track",
          .location = location_for(track.id, segment.id, field.name)});
      if (strategy == ColorStrategy::archive) {
        output.value = archive_value(archive, field.name);
        output.source = "archive";
        output.applied_archive = true;
      }
    }
    if (field.member == &ColorMetadata::primaries) resolved.primaries = output;
    if (field.member == &ColorMetadata::transfer) resolved.transfer = output;
    if (field.member == &ColorMetadata::matrix) resolved.matrix = output;
    if (field.member == &ColorMetadata::range) resolved.range = output;
  }
  return resolved;
}

}  // namespace

bool resolve_segment_color(const Track& track, const Segment& segment,
                           ColorStrategy strategy, const ArchiveColor& archive,
                           SegmentColor* out,
                           std::vector<Diagnostic>* diagnostics) {
  size_t missing = 0;
  size_t conflicts = 0;
  *out = resolve_one(track, segment, strategy, archive, &missing, &conflicts,
                     diagnostics);
  return true;
}

bool resolve_color_metadata(const Manifest& manifest, ColorStrategy strategy,
                            const ArchiveColor& archive, ColorReport* report,
                            std::vector<Diagnostic>* diagnostics) {
  for (const auto& track : manifest.tracks) {
    for (const auto& segment : track.segments) {
      report->segments.push_back(resolve_one(track, segment, strategy, archive,
                                             &report->missing_fields,
                                             &report->conflicts, diagnostics));
    }
  }
  return true;
}

}  // namespace media_restore

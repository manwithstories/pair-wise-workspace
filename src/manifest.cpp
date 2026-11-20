#include "relay/manifest.hpp"

#include <fstream>
#include <sstream>

#include "relay/hash.hpp"

namespace relay {
namespace {

constexpr const char* kSegmentKnownKeys[] = {
    "track",   "index",  "start_us",  "end_us",        "start_sequence",
    "end_sequence", "frame_count", "status", "status_reason", "content_hash",
    "rendition", "bitrate_bps", "random_access"};

constexpr const char* kTrackKnownKeys[] = {
    "id", "state", "dependency_chain", "epochs", "keyframes",
    "losses", "segments"};

constexpr const char* kTopKnownKeys[] = {
    "version", "input_digest", "config_digest", "timebase", "start_us",
    "end_us", "tracks", "downgrades", "unknown_fields"};

bool known(const char* const* list, std::size_t size, const std::string& key) {
  for (std::size_t i = 0; i < size; ++i) {
    if (key == list[i]) {
      return true;
    }
  }
  return false;
}

bool parse_status(const std::string& text, SegmentStatus& status) {
  if (text == "complete") {
    status = SegmentStatus::kComplete;
    return true;
  }
  if (text == "tail_truncated") {
    status = SegmentStatus::kTailTruncated;
    return true;
  }
  if (text == "gap") {
    status = SegmentStatus::kGap;
    return true;
  }
  if (text == "unseekable") {
    status = SegmentStatus::kUnseekable;
    return true;
  }
  return false;
}

template <typename T>
T number_or(const json::Value* value, T fallback) {
  if (value == nullptr || !value->is_number()) {
    return fallback;
  }
  return static_cast<T>(value->as_number());
}

std::string string_or(const json::Value* value, const std::string& fallback = {}) {
  if (value == nullptr || !value->is_string()) {
    return fallback;
  }
  return value->as_string();
}

json::Object unknown_from(const json::Value& object,
                          const char* const* known_keys,
                          std::size_t known_size) {
  json::Object out;
  for (const auto& [key, value] : object.as_object()) {
    if (!known(known_keys, known_size, key)) {
      out.emplace(key, value);
    }
  }
  return out;
}

void merge_unknown(json::Object& target, const json::Object& source) {
  for (const auto& [key, value] : source) {
    target.emplace(key, value);
  }
}

json::Value segment_to_json(const ManifestSegment& segment) {
  json::Object object;
  object["track"] = segment.track;
  object["index"] = segment.index;
  object["start_us"] = segment.start_us;
  object["end_us"] = segment.end_us;
  object["start_sequence"] =
      static_cast<std::uint64_t>(segment.start_sequence);
  object["end_sequence"] = static_cast<std::uint64_t>(segment.end_sequence);
  object["frame_count"] = static_cast<std::uint64_t>(segment.frame_count);
  object["status"] = segment.status;
  object["status_reason"] = segment.status_reason;
  object["content_hash"] = segment.content_hash;
  object["rendition"] = segment.rendition;
  object["bitrate_bps"] = segment.bitrate_bps;
  object["random_access"] = segment.random_access;
  for (const auto& [key, value] : segment.extensions) {
    object.emplace(key, value);
  }
  return json::Value(std::move(object));
}

json::Value track_to_json(const ManifestTrack& track) {
  json::Object object;
  object["id"] = track.id;
  object["state"] = track.state;
  object["dependency_chain"] = track.dependency_chain;
  object["epochs"] = track.epochs;
  object["keyframes"] = static_cast<std::uint64_t>(track.keyframes);
  object["losses"] = static_cast<std::uint64_t>(track.losses);
  json::Array segments;
  for (const auto& segment : track.segments) {
    segments.push_back(segment_to_json(segment));
  }
  object["segments"] = json::Value(std::move(segments));
  for (const auto& [key, value] : track.extensions) {
    object.emplace(key, value);
  }
  return json::Value(std::move(object));
}

json::Value downgrade_to_json(const DowngradeEvent& event) {
  json::Object object;
  object["segment"] = event.segment_index;
  object["from"] = event.from;
  object["to"] = event.to;
  object["estimate_bps"] = event.estimate_bps;
  object["cache_bytes"] = static_cast<std::uint64_t>(event.cache_bytes);
  object["reason"] = event.reason;
  return json::Value(std::move(object));
}

json::Value manifest_to_json(const Manifest& manifest) {
  json::Object object;
  object["version"] = manifest.version;
  object["input_digest"] = manifest.input_digest;
  object["config_digest"] = manifest.config_digest;
  json::Object timebase;
  timebase["num"] = manifest.timebase.fps_num;
  timebase["den"] = manifest.timebase.fps_den;
  object["timebase"] = json::Value(std::move(timebase));
  object["start_us"] = manifest.start_us;
  object["end_us"] = manifest.end_us;
  json::Array tracks;
  for (const auto& track : manifest.tracks) {
    tracks.push_back(track_to_json(track));
  }
  object["tracks"] = json::Value(std::move(tracks));
  json::Array downgrades;
  for (const auto& event : manifest.downgrades) {
    downgrades.push_back(downgrade_to_json(event));
  }
  object["downgrades"] = json::Value(std::move(downgrades));
  for (const auto& [key, value] : manifest.unknown_fields) {
    object.emplace(key, value);
  }
  return json::Value(std::move(object));
}

}  // namespace

std::string serialize(const Manifest& manifest) {
  return manifest_to_json(manifest).dump(2) + "\n";
}

ManifestParseResult parse_manifest(const std::string& text, Manifest& out) {
  ManifestParseResult result;
  json::Value root;
  json::ParseError error;
  if (!json::parse(text, root, error)) {
    result.error = "invalid JSON at byte " + std::to_string(error.offset) +
                   ": " + error.message;
    result.error_offset = error.offset;
    return result;
  }
  if (!root.is_object()) {
    result.error = "manifest root must be an object";
    return result;
  }
  out = Manifest{};
  out.version = static_cast<int>(number_or(root.find("version"), 1));
  out.input_digest = string_or(root.find("input_digest"));
  out.config_digest = string_or(root.find("config_digest"));
  const json::Value* timebase = root.find("timebase");
  if (timebase != nullptr && timebase->is_object()) {
    out.timebase.fps_num = static_cast<int>(number_or(timebase->find("num"), 30));
    out.timebase.fps_den = static_cast<int>(number_or(timebase->find("den"), 1));
  }
  out.start_us = number_or<Micros>(root.find("start_us"), 0);
  out.end_us = number_or<Micros>(root.find("end_us"), 0);

  const json::Value* tracks = root.find("tracks");
  if (tracks == nullptr || !tracks->is_array()) {
    result.error = "manifest is missing the 'tracks' array";
    result.ok = false;
    return result;
  }
  for (const auto& track_value : tracks->as_array()) {
    if (!track_value.is_object()) {
      result.isolated.push_back("track record is not an object");
      continue;
    }
    ManifestTrack track;
    track.id = string_or(track_value.find("id"));
    if (track.id.empty()) {
      result.isolated.push_back("track record without an id");
      continue;
    }
    track.state = string_or(track_value.find("state"), "ready");
    track.dependency_chain = string_or(track_value.find("dependency_chain"));
    track.epochs = static_cast<int>(number_or(track_value.find("epochs"), 1));
    track.keyframes = static_cast<std::size_t>(
        number_or<std::uint64_t>(track_value.find("keyframes"), 0));
    track.losses = static_cast<std::size_t>(
        number_or<std::uint64_t>(track_value.find("losses"), 0));
    const json::Value* segments = track_value.find("segments");
    if (segments != nullptr && segments->is_array()) {
      std::size_t index = 0;
      for (const auto& segment_value : segments->as_array()) {
        if (!segment_value.is_object()) {
          result.isolated.push_back(track.id + ": segment " +
                                    std::to_string(index) +
                                    " is not an object");
          ++index;
          continue;
        }
        ManifestSegment segment;
        segment.track = string_or(segment_value.find("track"), track.id);
        segment.index = number_or<std::uint64_t>(segment_value.find("index"), index);
        segment.start_us = number_or<Micros>(segment_value.find("start_us"), 0);
        segment.end_us = number_or<Micros>(segment_value.find("end_us"), 0);
        if (segment.end_us < segment.start_us) {
          result.isolated.push_back(track.id + ": segment " +
                                    std::to_string(index) +
                                    " has end before start; quarantined");
          ++index;
          continue;
        }
        segment.start_sequence = number_or<std::uint32_t>(
            segment_value.find("start_sequence"), 0);
        segment.end_sequence = number_or<std::uint32_t>(
            segment_value.find("end_sequence"), 0);
        segment.frame_count = static_cast<std::size_t>(
            number_or<std::uint64_t>(segment_value.find("frame_count"), 0));
        SegmentStatus status = SegmentStatus::kComplete;
        const std::string status_text = string_or(segment_value.find("status"));
        if (!parse_status(status_text, status)) {
          result.isolated.push_back(track.id + ": segment " +
                                    std::to_string(index) +
                                    " has unknown status '" + status_text + "'");
          ++index;
          continue;
        }
        segment.status = status_text;
        segment.status_reason = string_or(segment_value.find("status_reason"));
        segment.content_hash = string_or(segment_value.find("content_hash"));
        if (!segment.content_hash.empty() &&
            !is_hex_digest(segment.content_hash)) {
          result.isolated.push_back(track.id + ": segment " +
                                    std::to_string(index) +
                                    " has malformed content hash; quarantined");
          ++index;
          continue;
        }
        segment.rendition = string_or(segment_value.find("rendition"));
        segment.bitrate_bps = number_or<std::uint64_t>(
            segment_value.find("bitrate_bps"), 0);
        const json::Value* random_access = segment_value.find("random_access");
        segment.random_access =
            random_access != nullptr && random_access->type() == json::Type::kBool &&
            random_access->as_bool();
        (void)status;
        segment.extensions = unknown_from(
            segment_value, kSegmentKnownKeys,
            sizeof(kSegmentKnownKeys) / sizeof(kSegmentKnownKeys[0]));
        track.segments.push_back(std::move(segment));
        ++index;
      }
    }
    track.extensions = unknown_from(
        track_value, kTrackKnownKeys,
        sizeof(kTrackKnownKeys) / sizeof(kTrackKnownKeys[0]));
    out.tracks.push_back(std::move(track));
  }

  const json::Value* downgrades = root.find("downgrades");
  if (downgrades != nullptr && downgrades->is_array()) {
    for (const auto& value : downgrades->as_array()) {
      if (!value.is_object()) {
        result.isolated.push_back("downgrade record is not an object");
        continue;
      }
      DowngradeEvent event;
      event.segment_index =
          number_or<std::uint64_t>(value.find("segment"), 0);
      event.from = string_or(value.find("from"));
      event.to = string_or(value.find("to"));
      event.estimate_bps =
          number_or<std::uint64_t>(value.find("estimate_bps"), 0);
      event.cache_bytes = static_cast<std::size_t>(
          number_or<std::uint64_t>(value.find("cache_bytes"), 0));
      event.reason = string_or(value.find("reason"));
      out.downgrades.push_back(std::move(event));
    }
  }
  out.unknown_fields = unknown_from(
      root, kTopKnownKeys,
      sizeof(kTopKnownKeys) / sizeof(kTopKnownKeys[0]));
  const json::Value* nested_unknown = root.find("unknown_fields");
  if (nested_unknown != nullptr && nested_unknown->is_object()) {
    merge_unknown(out.unknown_fields, nested_unknown->as_object());
  }
  result.ok = true;
  return result;
}

ManifestParseResult load_manifest(const std::filesystem::path& path,
                                  Manifest& out) {
  ManifestParseResult result;
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    result.error = "cannot open manifest: " + path.string();
    return result;
  }
  std::ostringstream content;
  content << in.rdbuf();
  return parse_manifest(content.str(), out);
}

bool store_manifest(const std::filesystem::path& path, const Manifest& manifest,
                    std::string& error) {
  namespace fs = std::filesystem;
  const fs::path temporary = path.string() + ".tmp";
  {
    std::ofstream out(temporary, std::ios::binary | std::ios::trunc);
    if (!out) {
      error = "cannot create " + temporary.string();
      return false;
    }
    const std::string text = serialize(manifest);
    out.write(text.data(), static_cast<std::streamsize>(text.size()));
    if (!out) {
      error = "short write to " + temporary.string();
      return false;
    }
  }
  std::error_code ec;
  fs::rename(temporary, path, ec);
  if (ec) {
    error = "cannot replace " + path.string() + ": " + ec.message();
    return false;
  }
  return true;
}

std::string manifest_digest(const Manifest& manifest) {
  return sha256_hex(serialize(manifest));
}

}  // namespace relay

#include "media/index.hpp"

#include "media/sha256.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <string>

namespace mediaqc {
namespace {

namespace fs = std::filesystem;

Error make_error(ErrorCategory category, std::string code, std::string message) {
  Error error;
  error.category = category;
  error.code = std::move(code);
  error.message = std::move(message);
  return error;
}

}  // namespace

std::string MediaIndex::sidecar_path(const std::string& output_path) const {
  fs::path path(output_path);
  fs::path parent = path.parent_path();
  std::string stem = path.stem().string();
  return (parent / (stem + ".index.json")).string();
}

Json MediaIndex::to_json() const {
  Json root = Json::make_object();
  root.set("format", Json::make_string("mediaqc-index"));
  root.set("version", Json::make_int(1));
  root.set("frames_total", Json::make_int(static_cast<std::int64_t>(frames_total)));
  root.set("frames_repaired",
           Json::make_int(static_cast<std::int64_t>(frames_repaired)));
  root.set("frames_dropped",
           Json::make_int(static_cast<std::int64_t>(frames_dropped)));
  root.set("duration_ns", Json::make_int(duration_ns));
  root.set("output_sha256", Json::make_string(output_sha256));
  root.set("input_sha256", Json::make_string(input_sha256));

  Json track_array = Json::make_array();
  for (const IndexTrack& track : tracks) {
    Json item = Json::make_object();
    item.set("id", Json::make_string(track.id));
    item.set("kind", Json::make_string(std::string(to_string(track.kind))));
    item.set("codec", Json::make_string(track.codec));
    if (track.kind == TrackKind::Video) {
      item.set("width", Json::make_int(track.width));
      item.set("height", Json::make_int(track.height));
    } else {
      item.set("sample_rate", Json::make_int(track.sample_rate));
      item.set("channels", Json::make_int(track.channels));
    }
    Json rate = Json::make_object();
    rate.set("num", Json::make_int(track.rate.num));
    rate.set("den", Json::make_int(track.rate.den));
    item.set("rate", std::move(rate));
    item.set("entries", Json::make_int(static_cast<std::int64_t>(track.entries)));
    track_array.push(std::move(item));
  }
  root.set("tracks", std::move(track_array));

  Json entry_array = Json::make_array();
  for (const IndexEntry& entry : entries) {
    Json item = Json::make_object();
    item.set("time_ns", Json::make_int(entry.time_ns));
    item.set("frame", Json::make_int(static_cast<std::int64_t>(entry.frame)));
    item.set("track", Json::make_string(entry.track));
    item.set("packet", Json::make_int(static_cast<std::int64_t>(entry.output_packet)));
    item.set("offset", Json::make_int(static_cast<std::int64_t>(entry.payload_offset)));
    item.set("size", Json::make_int(static_cast<std::int64_t>(entry.payload_size)));
    item.set("keyframe", Json::make_bool(entry.keyframe));
    item.set("sha256", Json::make_string(to_hex(entry.sha256)));
    entry_array.push(std::move(item));
  }
  root.set("entries", std::move(entry_array));
  return root;
}

bool MediaIndex::write(const std::string& output_path, Error& error) const {
  const std::string target = sidecar_path(output_path);
  // Serialize first so a full disk leaves either the old file or a complete
  // new one, never a half-written JSON document.
  const std::string text = dump_json(to_json(), true) + "\n";
  const std::string temp = target + ".tmp";
  {
    std::ofstream out(temp, std::ios::binary | std::ios::trunc);
    if (!out) {
      error = make_error(ErrorCategory::kIo, "index_write_failed",
                         "cannot open " + temp);
      return false;
    }
    out.write(text.data(), static_cast<std::streamsize>(text.size()));
    if (!out) {
      error = make_error(ErrorCategory::kIo, "index_write_failed",
                         "cannot write " + temp);
      return false;
    }
  }
  std::error_code ec;
  fs::rename(temp, target, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "index_write_failed",
                       "cannot publish " + target + ": " + ec.message());
    return false;
  }
  return true;
}

bool load_index(const std::string& index_path, MediaIndex& out, Error& error) {
  std::ifstream in(index_path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kIo, "index_missing",
                       "cannot open index " + index_path);
    return false;
  }
  std::string text((std::istreambuf_iterator<char>(in)),
                   std::istreambuf_iterator<char>());
  JsonParseResult parsed = parse_json(text);
  if (!parsed.ok) {
    error = make_error(ErrorCategory::kCorruption, "index_invalid",
                       index_path + ": " + parsed.error);
    return false;
  }
  const Json& root = parsed.value;
  const Json* version = root.find("version");
  if (!version || version->as_int() != 1) {
    error = make_error(ErrorCategory::kCorruption, "index_version",
                       index_path + ": unsupported index version");
    return false;
  }
  MediaIndex index;
  const Json* total = root.find("frames_total");
  if (total) index.frames_total = static_cast<std::uint64_t>(total->as_int());
  const Json* repaired = root.find("frames_repaired");
  if (repaired) index.frames_repaired = static_cast<std::uint64_t>(repaired->as_int());
  const Json* duration = root.find("duration_ns");
  if (duration) index.duration_ns = duration->as_int();
  const Json* out_sha = root.find("output_sha256");
  if (out_sha) index.output_sha256 = out_sha->as_string();

  const Json* tracks = root.find("tracks");
  if (tracks && tracks->is_array()) {
    for (const Json& item : tracks->items()) {
      IndexTrack track;
      if (const Json* id = item.find("id")) track.id = id->as_string();
      if (const Json* kind = item.find("kind")) {
        if (auto parsed_kind = parse_track_kind(kind->as_string())) {
          track.kind = *parsed_kind;
        }
      }
      if (const Json* codec = item.find("codec")) track.codec = codec->as_string();
      if (const Json* width = item.find("width")) track.width = static_cast<int>(width->as_int());
      if (const Json* height = item.find("height")) track.height = static_cast<int>(height->as_int());
      if (const Json* rate = item.find("rate")) {
        const Json* num = rate->find("num");
        const Json* den = rate->find("den");
        if (num && den) {
          track.rate.num = static_cast<std::int32_t>(num->as_int());
          track.rate.den = static_cast<std::int32_t>(den->as_int());
        }
      }
      if (const Json* count = item.find("entries")) {
        track.entries = static_cast<std::size_t>(count->as_int());
      }
      index.tracks.push_back(std::move(track));
    }
  }

  const Json* entries = root.find("entries");
  if (entries && entries->is_array()) {
    index.entries.reserve(entries->items().size());
    for (const Json& item : entries->items()) {
      IndexEntry entry;
      if (const Json* time = item.find("time_ns")) entry.time_ns = time->as_int();
      if (const Json* frame = item.find("frame")) entry.frame = static_cast<std::uint64_t>(frame->as_int());
      if (const Json* track = item.find("track")) entry.track = track->as_string();
      if (const Json* packet = item.find("packet")) entry.output_packet = static_cast<std::uint64_t>(packet->as_int());
      if (const Json* offset = item.find("offset")) entry.payload_offset = static_cast<std::uint64_t>(offset->as_int());
      if (const Json* size = item.find("size")) entry.payload_size = static_cast<std::uint64_t>(size->as_int());
      if (const Json* key = item.find("keyframe")) entry.keyframe = key->as_bool();
      if (const Json* sha = item.find("sha256")) {
        if (auto digest = parse_hex_digest(sha->as_string())) entry.sha256 = *digest;
      }
      index.entries.push_back(std::move(entry));
    }
  }
  out = std::move(index);
  return true;
}

const IndexEntry* find_by_time(const MediaIndex& index, Ns time_ns) {
  if (index.entries.empty()) return nullptr;
  auto it = std::lower_bound(
      index.entries.begin(), index.entries.end(), time_ns,
      [](const IndexEntry& entry, Ns value) { return entry.time_ns < value; });
  if (it == index.entries.end()) return &index.entries.back();
  if (it == index.entries.begin()) return &*it;
  // Prefer the entry at or before the requested time.
  if (it->time_ns > time_ns) --it;
  return &*it;
}

const IndexEntry* find_by_frame(const MediaIndex& index, std::uint64_t frame) {
  if (index.entries.empty()) return nullptr;
  auto it = std::lower_bound(
      index.entries.begin(), index.entries.end(), frame,
      [](const IndexEntry& entry, std::uint64_t value) { return entry.frame < value; });
  if (it == index.entries.end()) return &index.entries.back();
  if (it == index.entries.begin()) return &*it;
  if (it->frame > frame) --it;
  return &*it;
}

}  // namespace mediaqc

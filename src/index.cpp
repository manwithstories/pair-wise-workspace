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

std::string MediaIndex::path_in(const std::string& dir) {
  return (fs::path(dir) / "index.json").string();
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
    item.set("frames", Json::make_int(static_cast<std::int64_t>(track.frames)));
    item.set("segments", Json::make_int(static_cast<std::int64_t>(track.segments)));
    item.set("entries", Json::make_int(static_cast<std::int64_t>(track.entries)));
    track_array.push(std::move(item));
  }
  root.set("tracks", std::move(track_array));

  Json segment_array = Json::make_array();
  for (const SegmentInfo& segment : segments) {
    Json item = Json::make_object();
    item.set("track", Json::make_string(segment.track));
    item.set("path", Json::make_string(segment.path));
    item.set("first_frame", Json::make_int(static_cast<std::int64_t>(segment.first_frame)));
    item.set("frames", Json::make_int(static_cast<std::int64_t>(segment.frames)));
    item.set("start_ns", Json::make_int(segment.start_ns));
    item.set("end_ns", Json::make_int(segment.end_ns));
    item.set("sha256", Json::make_string(segment.sha256));
    item.set("bytes", Json::make_int(static_cast<std::int64_t>(segment.bytes)));
    segment_array.push(std::move(item));
  }
  root.set("segments", std::move(segment_array));

  Json entry_array = Json::make_array();
  for (const IndexEntry& entry : entries) {
    Json item = Json::make_object();
    item.set("time_ns", Json::make_int(entry.time_ns));
    item.set("frame", Json::make_int(static_cast<std::int64_t>(entry.frame)));
    item.set("frame_index",
             Json::make_int(static_cast<std::int64_t>(entry.frame_index)));
    item.set("track", Json::make_string(entry.track));
    item.set("segment", Json::make_int(static_cast<std::int64_t>(entry.segment)));
    item.set("offset", Json::make_int(static_cast<std::int64_t>(entry.payload_offset)));
    item.set("size", Json::make_int(static_cast<std::int64_t>(entry.payload_size)));
    item.set("keyframe", Json::make_bool(entry.keyframe));
    item.set("sha256", Json::make_string(to_hex(entry.sha256)));
    entry_array.push(std::move(item));
  }
  root.set("entries", std::move(entry_array));
  return root;
}

bool MediaIndex::write(const std::string& dir, Error& error) const {
  std::error_code ec;
  fs::create_directories(dir, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "index_write_failed",
                       "cannot create " + dir + ": " + ec.message());
    return false;
  }
  const std::string target = path_in(dir);
  const std::string temp = target + ".tmp";
  const std::string text = dump_json(to_json(), true) + "\n";
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
  fs::rename(temp, target, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "index_write_failed",
                       "cannot publish " + target + ": " + ec.message());
    return false;
  }
  return true;
}

bool load_index(const std::string& dir, MediaIndex& out, Error& error) {
  const std::string path = MediaIndex::path_in(dir);
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kIo, "index_missing",
                       "cannot open index " + path);
    return false;
  }
  std::string text((std::istreambuf_iterator<char>(in)),
                   std::istreambuf_iterator<char>());
  JsonParseResult parsed = parse_json(text);
  if (!parsed.ok) {
    error = make_error(ErrorCategory::kCorruption, "index_invalid",
                       path + ": " + parsed.error);
    return false;
  }
  const Json& root = parsed.value;
  const Json* version = root.find("version");
  if (!version || version->as_int() != 1) {
    error = make_error(ErrorCategory::kCorruption, "index_version",
                       path + ": unsupported index version");
    return false;
  }
  MediaIndex index;
  auto read_u64 = [&](const char* key, std::uint64_t& field) {
    if (const Json* v = root.find(key)) field = static_cast<std::uint64_t>(v->as_int());
  };
  read_u64("frames_total", index.frames_total);
  read_u64("frames_repaired", index.frames_repaired);
  read_u64("frames_dropped", index.frames_dropped);
  if (const Json* v = root.find("duration_ns")) index.duration_ns = v->as_int();
  if (const Json* v = root.find("input_sha256")) index.input_sha256 = v->as_string();

  if (const Json* tracks = root.find("tracks"); tracks && tracks->is_array()) {
    for (const Json& item : tracks->items()) {
      IndexTrack track;
      if (const Json* v = item.find("id")) track.id = v->as_string();
      if (const Json* v = item.find("kind")) {
        if (auto parsed_kind = parse_track_kind(v->as_string())) track.kind = *parsed_kind;
      }
      if (const Json* v = item.find("codec")) track.codec = v->as_string();
      if (const Json* v = item.find("width")) track.width = static_cast<int>(v->as_int());
      if (const Json* v = item.find("height")) track.height = static_cast<int>(v->as_int());
      if (const Json* v = item.find("sample_rate")) track.sample_rate = static_cast<int>(v->as_int());
      if (const Json* v = item.find("channels")) track.channels = static_cast<int>(v->as_int());
      if (const Json* rate = item.find("rate")) {
        if (const Json* n = rate->find("num")) track.rate.num = static_cast<std::int32_t>(n->as_int());
        if (const Json* d = rate->find("den")) track.rate.den = static_cast<std::int32_t>(d->as_int());
      }
      if (const Json* v = item.find("frames")) track.frames = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("segments")) track.segments = static_cast<std::size_t>(v->as_int());
      if (const Json* v = item.find("entries")) track.entries = static_cast<std::size_t>(v->as_int());
      index.tracks.push_back(std::move(track));
    }
  }

  if (const Json* segments = root.find("segments"); segments && segments->is_array()) {
    for (const Json& item : segments->items()) {
      SegmentInfo segment;
      if (const Json* v = item.find("track")) segment.track = v->as_string();
      if (const Json* v = item.find("path")) segment.path = v->as_string();
      // Reject a segment path that escapes the output directory (an attacker
      // or a corrupt index must not make verify read arbitrary files).
      fs::path resolved = fs::weakly_canonical(fs::path(dir) / segment.path);
      fs::path base = fs::weakly_canonical(fs::path(dir));
      auto mismatch = std::mismatch(base.begin(), base.end(), resolved.begin(), resolved.end());
      if (mismatch.first != base.end()) {
        error = make_error(ErrorCategory::kCorruption, "index_path_escape",
                           "segment path escapes output directory: " + segment.path);
        return false;
      }
      if (const Json* v = item.find("first_frame")) segment.first_frame = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("frames")) segment.frames = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("start_ns")) segment.start_ns = v->as_int();
      if (const Json* v = item.find("end_ns")) segment.end_ns = v->as_int();
      if (const Json* v = item.find("sha256")) segment.sha256 = v->as_string();
      if (const Json* v = item.find("bytes")) segment.bytes = static_cast<std::uint64_t>(v->as_int());
      index.segments.push_back(std::move(segment));
    }
  }

  if (const Json* entries = root.find("entries"); entries && entries->is_array()) {
    index.entries.reserve(entries->items().size());
    for (const Json& item : entries->items()) {
      IndexEntry entry;
      if (const Json* v = item.find("time_ns")) entry.time_ns = v->as_int();
      if (const Json* v = item.find("frame")) entry.frame = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("frame_index")) entry.frame_index = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("track")) entry.track = v->as_string();
      if (const Json* v = item.find("segment")) entry.segment = static_cast<std::size_t>(v->as_int());
      if (const Json* v = item.find("offset")) entry.payload_offset = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("size")) entry.payload_size = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("keyframe")) entry.keyframe = v->as_bool();
      if (const Json* v = item.find("sha256")) {
        if (auto digest = parse_hex_digest(v->as_string())) entry.sha256 = *digest;
      }
      index.entries.push_back(std::move(entry));
    }
  }
  out = std::move(index);
  return true;
}

namespace {

const IndexEntry* find_range(const MediaIndex& index, const std::string& track,
                             bool by_time, Ns time_ns, std::uint64_t frame) {
  auto begin = index.entries.begin();
  auto end = index.entries.end();
  // Entries are grouped by track; narrow to that contiguous block first.
  auto lower = std::lower_bound(
      begin, end, track,
      [](const IndexEntry& entry, const std::string& value) { return entry.track < value; });
  auto upper = std::upper_bound(
      lower, end, track,
      [](const std::string& value, const IndexEntry& entry) { return value < entry.track; });
  if (lower == upper) return nullptr;
  if (by_time) {
    auto it = std::lower_bound(
        lower, upper, time_ns,
        [](const IndexEntry& entry, Ns value) { return entry.time_ns < value; });
    if (it == upper) return &*(upper - 1);
    if (it != lower && it->time_ns > time_ns) --it;
    return &*it;
  }
  auto it = std::lower_bound(
      lower, upper, frame,
      [](const IndexEntry& entry, std::uint64_t value) { return entry.frame < value; });
  if (it == upper) return &*(upper - 1);
  if (it != lower && it->frame > frame) --it;
  return &*it;
}

}  // namespace

const IndexEntry* find_by_time(const MediaIndex& index, const std::string& track,
                               Ns time_ns) {
  return find_range(index, track, true, time_ns, 0);
}

const IndexEntry* find_by_frame(const MediaIndex& index, const std::string& track,
                                std::uint64_t frame) {
  return find_range(index, track, false, 0, frame);
}

}  // namespace mediaqc

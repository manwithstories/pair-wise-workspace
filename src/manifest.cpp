#include "manifest.hpp"

#include <string_view>

#include "file_io.hpp"
#include "json.hpp"

namespace media_restore {
namespace {

Diagnostic make(ErrorCode code, std::string message, const Location& location) {
  Diagnostic diagnostic;
  diagnostic.code = code;
  diagnostic.message = std::move(message);
  diagnostic.location = location;
  return diagnostic;
}

bool read_string(const Json& object, std::string_view key, std::string* out,
                 bool required, const Location& loc,
                 std::vector<Diagnostic>* diagnostics) {
  const Json* value = object.find(key);
  if (value == nullptr) {
    if (required) {
      Location where = loc;
      where.field = std::string(key);
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "missing required field", where));
      return false;
    }
    return true;
  }
  if (!value->is_string()) {
    Location where = loc;
    where.field = std::string(key);
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "field must be a string", where));
    return false;
  }
  *out = value->as_string();
  return true;
}

bool read_integer(const Json& object, std::string_view key, int64_t* out,
                  bool required, const Location& loc,
                  std::vector<Diagnostic>* diagnostics) {
  const Json* value = object.find(key);
  if (value == nullptr) {
    if (required) {
      Location where = loc;
      where.field = std::string(key);
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "missing required field", where));
      return false;
    }
    return true;
  }
  if (!value->is_integer()) {
    Location where = loc;
    where.field = std::string(key);
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "field must be an integer", where));
    return false;
  }
  *out = value->as_integer();
  return true;
}

bool read_time(const Json& object, std::string_view key, Rational* out,
               const Location& loc, std::vector<Diagnostic>* diagnostics) {
  std::string text;
  if (!read_string(object, key, &text, true, loc, diagnostics)) return false;
  if (!Rational::parse(text, out) || !out->valid() || out->num < 0) {
    Location where = loc;
    where.field = std::string(key);
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "time must be a non-negative 'num/den' string",
                                where));
    return false;
  }
  return true;
}

bool parse_color(const Json& value, ColorMetadata* out, const Location& loc,
                 std::vector<Diagnostic>* diagnostics) {
  if (!value.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "color must be an object", loc));
    return false;
  }
  const Json* source = value.find("source");
  if (source != nullptr && source->is_string()) out->source = source->as_string();
  const struct {
    const char* key;
    std::optional<std::string>* slot;
  } fields[] = {{"primaries", &out->primaries},
                {"transfer", &out->transfer},
                {"matrix", &out->matrix},
                {"range", &out->range}};
  for (const auto& field : fields) {
    const Json* entry = value.find(field.key);
    if (entry == nullptr) continue;
    if (!entry->is_string()) {
      Location where = loc;
      where.field = field.key;
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "color field must be a string", where));
      return false;
    }
    *field.slot = entry->as_string();
  }
  return true;
}

bool parse_index(const Json& value, KeyframeIndex* out, const Location& loc,
                 std::vector<Diagnostic>* diagnostics) {
  out->present = true;
  if (!value.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "index must be an object", loc));
    return false;
  }
  if (!read_string(value, "path", &out->path, true, loc, diagnostics)) return false;
  int64_t version = 0;
  if (!read_integer(value, "version", &version, true, loc, diagnostics)) return false;
  if (version < kIndexVersionMin || version > kIndexVersionMax) {
    Location where = loc;
    where.field = "version";
    diagnostics->push_back(make(ErrorCode::index_version,
                                "unsupported index version " + std::to_string(version),
                                where));
    return false;
  }
  out->version = static_cast<int>(version);
  const Json* entries = value.find("entries");
  if (entries == nullptr) return true;  // entries may live in the index file
  if (!entries->is_array()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "index entries must be an array", loc));
    return false;
  }
  for (const Json& item : entries->items()) {
    if (!item.is_object()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "index entry must be an object", loc));
      return false;
    }
    IndexEntry entry;
    if (!read_integer(item, "tick", &entry.tick, true, loc, diagnostics) ||
        !read_integer(item, "offset", &entry.offset, true, loc, diagnostics)) {
      return false;
    }
    // Keyframes accept either a JSON boolean or an integer flag.
    const Json* keyframe = item.find("keyframe");
    if (keyframe != nullptr) {
      if (keyframe->is_bool()) {
        entry.keyframe = keyframe->as_bool();
      } else if (keyframe->is_integer()) {
        entry.keyframe = keyframe->as_integer() != 0;
      } else {
        Location where = loc;
        where.field = "keyframe";
        diagnostics->push_back(make(ErrorCode::manifest_schema,
                                    "keyframe must be a boolean", where));
        return false;
      }
    }
    out->entries.push_back(entry);
  }
  return true;
}

bool parse_audio(const Json& value, AudioInfo* out, const Location& loc,
                 std::vector<Diagnostic>* diagnostics) {
  if (!value.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "audio must be an object", loc));
    return false;
  }
  int64_t rate = 0;
  if (!read_integer(value, "sample_rate", &rate, true, loc, diagnostics)) return false;
  if (rate <= 0 || rate > 768000) {
    Location where = loc;
    where.field = "sample_rate";
    diagnostics->push_back(make(ErrorCode::audio_unsupported_rate,
                                "sample rate out of supported range", where));
    return false;
  }
  out->sample_rate = static_cast<int>(rate);
  if (!read_string(value, "layout", &out->layout, true, loc, diagnostics)) return false;
  if (!read_string(value, "media", &out->media, false, loc, diagnostics)) return false;
  std::string offset;
  if (read_string(value, "offset", &offset, true, loc, diagnostics)) {
    if (!Rational::parse(offset, &out->offset) || !out->offset.valid()) {
      Location where = loc;
      where.field = "offset";
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "audio offset must be a 'num/den' string", where));
      return false;
    }
  } else {
    return false;
  }
  return true;
}

bool parse_segment(const Json& value, Segment* out, TrackKind kind,
                   const Location& loc, std::vector<Diagnostic>* diagnostics) {
  if (!value.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "segment must be an object", loc));
    return false;
  }
  Location here = loc;
  if (!read_string(value, "id", &out->id, true, here, diagnostics)) return false;
  here.segment = out->id;
  if (!read_time(value, "start", &out->start, here, diagnostics) ||
      !read_time(value, "duration", &out->duration, here, diagnostics) ||
      !read_string(value, "media", &out->media, true, here, diagnostics)) {
    return false;
  }
  std::string codec;
  if (!read_string(value, "codec", &codec, kind != TrackKind::audio, here,
                   diagnostics)) {
    return false;
  }
  if (!codec.empty() && !parse_codec(codec, &out->codec)) {
    Location where = here;
    where.field = "codec";
    diagnostics->push_back(make(ErrorCode::unknown_codec,
                                "unknown codec marker '" + codec + "'", where));
    return false;
  }
  const Json* index = value.find("index");
  if (index != nullptr && !parse_index(*index, &out->index, here, diagnostics)) {
    return false;
  }
  const Json* color = value.find("color");
  if (color != nullptr && !parse_color(*color, &out->color, here, diagnostics)) {
    return false;
  }
  const Json* audio = value.find("audio");
  if (audio != nullptr) {
    AudioInfo info;
    if (!parse_audio(*audio, &info, here, diagnostics)) return false;
    out->audio = std::move(info);
  }
  const Json* quarantine = value.find("quarantine");
  if (quarantine != nullptr) {
    if (!quarantine->is_object()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "quarantine must be an object", here));
      return false;
    }
    std::string code;
    if (!read_string(*quarantine, "expected_code", &code, true, here, diagnostics)) {
      return false;
    }
    ErrorCode parsed = ErrorCode::internal;
    if (!parse_error_code(code, &parsed) || exit_code_for(parsed) != 2) {
      Location where = here;
      where.field = "expected_code";
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "expected_code must be a media validation "
                                  "error (exit code 2), got '" + code + "'",
                                  where));
      return false;
    }
    out->expected_code = parsed;
  }
  return true;
}

}  // namespace

const char* codec_name(Codec codec) {
  switch (codec) {
    case Codec::h264: return "h264";
    case Codec::h265: return "h265";
    case Codec::av1: return "av1";
  }
  return "unknown";
}

const char* track_kind_name(TrackKind kind) {
  return kind == TrackKind::video ? "video" : "audio";
}

bool parse_codec(std::string_view text, Codec* out) {
  if (text == "h264") {
    *out = Codec::h264;
    return true;
  }
  if (text == "h265" || text == "hevc") {
    *out = Codec::h265;
    return true;
  }
  if (text == "av1") {
    *out = Codec::av1;
    return true;
  }
  return false;
}

bool parse_track_kind(std::string_view text, TrackKind* out) {
  if (text == "video") {
    *out = TrackKind::video;
    return true;
  }
  if (text == "audio") {
    *out = TrackKind::audio;
    return true;
  }
  return false;
}

const Track* Manifest::find_track(std::string_view id) const {
  for (const auto& track : tracks) {
    if (track.id == id) return &track;
  }
  return nullptr;
}

const Segment* Manifest::find_segment(std::string_view track_id,
                                     std::string_view segment_id) const {
  const Track* track = find_track(track_id);
  if (track == nullptr) return nullptr;
  for (const auto& segment : track->segments) {
    if (segment.id == segment_id) return &segment;
  }
  return nullptr;
}

Rational Manifest::segment_end(const Segment& segment) const {
  Rational end;
  if (!Rational::add(segment.start, segment.duration, &end)) return segment.start;
  return end;
}

bool parse_manifest(std::string_view text, Manifest* out,
                    std::vector<Diagnostic>* diagnostics) {
  std::string error;
  size_t offset = 0;
  const Json root = Json::parse(text, &error, &offset);
  if (!error.empty()) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::json_parse;
    diagnostic.message = error;
    diagnostic.location.byte_offset = static_cast<int64_t>(offset);
    diagnostics->push_back(std::move(diagnostic));
    return false;
  }
  if (!root.is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "manifest root must be an object", {}));
    return false;
  }
  const size_t before = diagnostics->size();
  Location root_loc;
  Manifest manifest;
  std::string format;
  if (!read_string(root, "format", &format, true, root_loc, diagnostics)) return false;
  if (format != "media-archive-manifest") {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "unsupported manifest format '" + format + "'",
                                {.field = "format"}));
    return false;
  }
  int64_t version = 0;
  if (!read_integer(root, "version", &version, true, root_loc, diagnostics)) return false;
  if (version != kManifestVersion) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "unsupported manifest version " + std::to_string(version),
                                {.field = "version"}));
    return false;
  }
  manifest.version = static_cast<int>(version);
  const Json* time_base = root.find("time_base");
  if (time_base == nullptr || !time_base->is_object()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "time_base object is required", {.field = "time_base"}));
    return false;
  }
  std::string num_text;
  std::string den_text;
  const auto read_time_base_part = [&](const char* key, std::string* out) {
    const Json* part = time_base->find(key);
    Location where;
    where.field = "time_base";
    if (part == nullptr) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "time_base needs num and den", where));
      return false;
    }
    if (part->is_string()) {
      *out = part->as_string();
      return true;
    }
    if (part->is_integer()) {
      *out = std::to_string(part->as_integer());
      return true;
    }
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "time_base component must be an integer", where));
    return false;
  };
  if (!read_time_base_part("num", &num_text) ||
      !read_time_base_part("den", &den_text)) {
    return false;
  }
  Rational tb;
  if (!Rational::parse(num_text + "/" + den_text, &tb) || !tb.valid() || tb.num <= 0) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "time_base must be a positive rational",
                                {.field = "time_base"}));
    return false;
  }
  manifest.time_base = tb;

  const Json* tracks = root.find("tracks");
  if (tracks == nullptr || !tracks->is_array() || tracks->items().empty()) {
    diagnostics->push_back(make(ErrorCode::manifest_schema,
                                "manifest needs a non-empty tracks array",
                                {.field = "tracks"}));
    return false;
  }
  if (tracks->items().size() > Limits{}.max_tracks) {
    diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                "too many tracks", {.field = "tracks"}));
    return false;
  }
  std::vector<std::string> track_ids;
  for (const Json& track_json : tracks->items()) {
    Track track;
    Location loc;
    if (!track_json.is_object()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "track must be an object", loc));
      return false;
    }
    if (!read_string(track_json, "id", &track.id, true, loc, diagnostics)) return false;
    loc.track = track.id;
    for (const auto& existing : track_ids) {
      if (existing == track.id) {
        diagnostics->push_back(make(ErrorCode::manifest_schema,
                                    "duplicate track id", loc));
        return false;
      }
    }
    track_ids.push_back(track.id);
    std::string kind;
    if (!read_string(track_json, "kind", &kind, true, loc, diagnostics)) return false;
    if (!parse_track_kind(kind, &track.kind)) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "unknown track kind '" + kind + "'",
                                  {.track = track.id, .field = "kind"}));
      return false;
    }
    const Json* depends = track_json.find("depends_on");
    if (depends != nullptr) {
      if (!depends->is_array()) {
        diagnostics->push_back(make(ErrorCode::manifest_schema,
                                    "depends_on must be an array",
                                    {.track = track.id, .field = "depends_on"}));
        return false;
      }
      for (const Json& dependency : depends->items()) {
        if (!dependency.is_string()) {
          diagnostics->push_back(make(ErrorCode::manifest_schema,
                                      "dependency must be a track id string",
                                      {.track = track.id, .field = "depends_on"}));
          return false;
        }
        track.depends_on.push_back(dependency.as_string());
      }
    }
    const Json* segments = track_json.find("segments");
    if (segments == nullptr || !segments->is_array()) {
      diagnostics->push_back(make(ErrorCode::manifest_schema,
                                  "track needs a segments array", loc));
      return false;
    }
    if (segments->items().size() > Limits{}.max_segments_per_track) {
      diagnostics->push_back(make(ErrorCode::limit_exceeded,
                                  "too many segments in track", loc));
      return false;
    }
    std::vector<std::string> segment_ids;
    for (const Json& segment_json : segments->items()) {
      Segment segment;
      if (!parse_segment(segment_json, &segment, track.kind, loc, diagnostics)) {
        return false;
      }
      for (const auto& existing : segment_ids) {
        if (existing == segment.id) {
          diagnostics->push_back(make(ErrorCode::manifest_schema,
                                      "duplicate segment id",
                                      {.track = track.id, .segment = segment.id}));
          return false;
        }
      }
      segment_ids.push_back(segment.id);
      track.segments.push_back(std::move(segment));
    }
    manifest.tracks.push_back(std::move(track));
  }
  if (diagnostics->size() != before) return false;
  *out = std::move(manifest);
  return true;
}

namespace {

Json time_json(const Rational& time) {
  return Json(time.to_string());
}

Json color_json(const ColorMetadata& color) {
  Json out = Json::object();
  if (color.primaries) out.set("primaries", Json(*color.primaries));
  if (color.transfer) out.set("transfer", Json(*color.transfer));
  if (color.matrix) out.set("matrix", Json(*color.matrix));
  if (color.range) out.set("range", Json(*color.range));
  if (!color.source.empty()) out.set("source", Json(color.source));
  return out;
}

Json index_json(const KeyframeIndex& index) {
  Json out = Json::object();
  out.set("path", Json(index.path));
  out.set("version", Json(static_cast<int64_t>(index.version)));
  Json entries = Json::array();
  for (const auto& entry : index.entries) {
    Json item = Json::object();
    item.set("tick", Json(entry.tick));
    item.set("offset", Json(entry.offset));
    item.set("keyframe", Json(entry.keyframe));
    entries.push(std::move(item));
  }
  out.set("entries", std::move(entries));
  return out;
}

Json segment_json(const Segment& segment) {
  Json out = Json::object();
  out.set("id", Json(segment.id));
  out.set("start", time_json(segment.start));
  out.set("duration", time_json(segment.duration));
  out.set("media", Json(segment.media));
  out.set("codec", Json(codec_name(segment.codec)));
  if (segment.expected_code) {
    Json quarantine = Json::object();
    quarantine.set("expected_code", Json(error_code_name(*segment.expected_code)));
    out.set("quarantine", std::move(quarantine));
  }
  if (segment.index.present) out.set("index", index_json(segment.index));
  out.set("color", color_json(segment.color));
  if (segment.audio) {
    Json audio = Json::object();
    audio.set("sample_rate", Json(static_cast<int64_t>(segment.audio->sample_rate)));
    audio.set("layout", Json(segment.audio->layout));
    audio.set("offset", time_json(segment.audio->offset));
    if (!segment.audio->media.empty()) audio.set("media", Json(segment.audio->media));
    out.set("audio", std::move(audio));
  }
  return out;
}

}  // namespace

std::string serialize_manifest(const Manifest& manifest) {
  Json root = Json::object();
  root.set("format", Json(manifest.format));
  root.set("version", Json(static_cast<int64_t>(manifest.version)));
  Json time_base = Json::object();
  time_base.set("num", Json(std::to_string(manifest.time_base.num)));
  time_base.set("den", Json(std::to_string(manifest.time_base.den)));
  root.set("time_base", std::move(time_base));
  Json tracks = Json::array();
  for (const auto& track : manifest.tracks) {
    Json track_json = Json::object();
    track_json.set("id", Json(track.id));
    track_json.set("kind", Json(track_kind_name(track.kind)));
    Json depends = Json::array();
    for (const auto& dependency : track.depends_on) {
      depends.push(Json(dependency));
    }
    track_json.set("depends_on", std::move(depends));
    Json segments = Json::array();
    for (const auto& segment : track.segments) {
      segments.push(segment_json(segment));
    }
    track_json.set("segments", std::move(segments));
    tracks.push(std::move(track_json));
  }
  root.set("tracks", std::move(tracks));
  return root.dump();
}

bool load_manifest(const std::filesystem::path& path, const Limits& limits,
                   Manifest* out, std::vector<Diagnostic>* diagnostics) {
  std::string text;
  Diagnostic error;
  if (!read_file(path, limits.max_manifest_bytes, &text, &error)) {
    error.location.field = "manifest";
    diagnostics->push_back(std::move(error));
    return false;
  }
  if (!parse_manifest(text, out, diagnostics)) return false;
  out->source_path = path;
  return true;
}

}  // namespace media_restore

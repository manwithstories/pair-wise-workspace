#include "media/delivery.hpp"

#include <cerrno>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <set>
#include <string>
#include <vector>

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

void set_offset(Error& error, std::uint64_t offset) {
  error.offset = offset;
  error.has_offset = true;
}

// Reads a string member, returning nullopt when absent and recording a type
// error when present but not a string.
std::optional<std::string> get_string(const Json& object, std::string_view key,
                                      Error& error, const std::string& where) {
  const Json* value = object.find(key);
  if (!value) {
    return std::nullopt;
  }
  if (!value->is_string()) {
    error = make_error(ErrorCategory::kInvalidInput, "field_type",
                       where + ": field '" + std::string(key) +
                           "' must be a string");
    return std::nullopt;
  }
  return value->as_string();
}

std::optional<std::int64_t> get_int(const Json& object, std::string_view key,
                                    Error& error, const std::string& where) {
  const Json* value = object.find(key);
  if (!value) {
    return std::nullopt;
  }
  if (!value->is_int()) {
    error = make_error(ErrorCategory::kInvalidInput, "field_type",
                       where + ": field '" + std::string(key) +
                           "' must be an integer");
    return std::nullopt;
  }
  return value->as_int();
}

bool get_string_array(const Json& object, std::string_view key,
                      std::vector<std::string>& out, Error& error,
                      const std::string& where) {
  const Json* value = object.find(key);
  if (!value) {
    return true;
  }
  if (!value->is_array()) {
    error = make_error(ErrorCategory::kInvalidInput, "field_type",
                       where + ": field '" + std::string(key) +
                           "' must be an array of strings");
    return false;
  }
  for (const Json& item : value->items()) {
    if (!item.is_string()) {
      error = make_error(ErrorCategory::kInvalidInput, "field_type",
                         where + ": field '" + std::string(key) +
                             "' must contain only strings");
      return false;
    }
    out.push_back(item.as_string());
  }
  return true;
}

bool parse_clock(const Json& object, ClockSpec& clock, Error& error,
                 const std::string& where) {
  std::string kind = "ns";
  if (auto value = get_string(object, "kind", error, where)) {
    kind = *value;
  } else if (!error.message.empty()) {
    return false;
  }
  if (kind != "ticks") {
    clock.hz = static_cast<std::uint32_t>(kNsPerSecond);
    clock.bits = 0;
    return true;
  }
  auto hz = get_int(object, "hz", error, where);
  if (!error.message.empty()) return false;
  if (hz && (*hz <= 0 || *hz > static_cast<std::int64_t>(0xffffffffLL))) {
    error = make_error(ErrorCategory::kInvalidInput, "clock_hz",
                       where + ": clock hz out of range");
    return false;
  }
  if (hz) clock.hz = static_cast<std::uint32_t>(*hz);
  auto bits = get_int(object, "bits", error, where);
  if (!error.message.empty()) return false;
  if (bits) {
    if (*bits < 0 || *bits > 64) {
      error = make_error(ErrorCategory::kInvalidInput, "clock_bits",
                         where + ": clock bits must be 0 through 64");
      return false;
    }
    clock.bits = static_cast<std::uint8_t>(*bits);
  }
  return true;
}

bool parse_rate(const Json& object, Rational& rate, Error& error,
                const std::string& where) {
  const Json* value = object.find("rate");
  if (!value) {
    rate = Rational{0, 1};
    return true;
  }
  if (!value->is_object()) {
    error = make_error(ErrorCategory::kInvalidInput, "field_type",
                       where + ": 'rate' must be an object with num/den");
    return false;
  }
  auto num = get_int(*value, "num", error, where);
  if (!error.message.empty()) return false;
  auto den = get_int(*value, "den", error, where);
  if (!error.message.empty()) return false;
  if (!num || !den || *num <= 0 || *den <= 0) {
    error = make_error(ErrorCategory::kInvalidInput, "bad_rate",
                       where + ": 'rate' must have positive num and den");
    return false;
  }
  rate.num = static_cast<std::int32_t>(*num);
  rate.den = static_cast<std::int32_t>(*den);
  return true;
}

bool parse_track(const Json& object, std::size_t index, DeliveryTrack& out,
                 Error& error) {
  const std::string where = "track[" + std::to_string(index) + "]";
  if (!object.is_object()) {
    error = make_error(ErrorCategory::kInvalidInput, "track_type",
                       where + " must be an object");
    return false;
  }

  auto id = get_string(object, "id", error, where);
  if (!error.message.empty()) return false;
  if (!id || id->empty()) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": missing required field 'id'");
    return false;
  }
  out.desc.id = *id;

  auto kind_text = get_string(object, "kind", error, where);
  if (!error.message.empty()) return false;
  if (!kind_text) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": missing required field 'kind'");
    return false;
  }
  auto kind = parse_track_kind(*kind_text);
  if (!kind) {
    error = make_error(ErrorCategory::kInvalidInput, "bad_kind",
                       where + ": unknown track kind '" + *kind_text + "'");
    return false;
  }
  out.desc.kind = *kind;

  auto codec = get_string(object, "codec", error, where);
  if (!error.message.empty()) return false;
  if (!codec || codec->empty()) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": missing required field 'codec'");
    return false;
  }
  out.desc.codec = *codec;

  if (!parse_rate(object, out.desc.rate, error, where)) return false;
  if (out.desc.kind == TrackKind::Video && !out.desc.rate.valid()) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": video track requires 'rate'");
    return false;
  }

  auto pix = get_string(object, "pix_fmt", error, where);
  if (!error.message.empty()) return false;
  if (pix) {
    auto parsed = parse_pixel_format(*pix);
    if (!parsed) {
      error = make_error(ErrorCategory::kInvalidInput, "bad_pixel_format",
                         where + ": unsupported pixel format '" + *pix + "'");
      return false;
    }
    out.desc.pix_fmt = *pix;
  }
  auto width = get_int(object, "width", error, where);
  if (!error.message.empty()) return false;
  auto height = get_int(object, "height", error, where);
  if (!error.message.empty()) return false;
  out.desc.width = width ? static_cast<int>(*width) : 0;
  out.desc.height = height ? static_cast<int>(*height) : 0;
  if (out.desc.kind == TrackKind::Video &&
      (out.desc.width <= 0 || out.desc.height <= 0)) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": video track requires positive width/height");
    return false;
  }

  auto rate_hz = get_int(object, "sample_rate", error, where);
  if (!error.message.empty()) return false;
  out.desc.sample_rate = rate_hz ? static_cast<int>(*rate_hz) : 0;
  auto channels = get_int(object, "channels", error, where);
  if (!error.message.empty()) return false;
  out.desc.channels = channels ? static_cast<int>(*channels) : 0;
  if (out.desc.kind == TrackKind::Audio &&
      (out.desc.sample_rate <= 0 || out.desc.channels <= 0)) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where +
                           ": audio track requires positive sample_rate/channels");
    return false;
  }

  const Json* clock = object.find("clock");
  if (clock) {
    if (!clock->is_object()) {
      error = make_error(ErrorCategory::kInvalidInput, "field_type",
                         where + ": 'clock' must be an object");
      return false;
    }
    if (!parse_clock(*clock, out.desc.clock, error, where)) return false;
  }

  if (!get_string_array(object, "depends_on", out.desc.depends_on, error, where)) {
    return false;
  }
  if (!get_string_array(object, "required_fields", out.desc.required_fields,
                        error, where)) {
    return false;
  }
  if (!get_string_array(object, "optional_fields", out.desc.optional_fields,
                        error, where)) {
    return false;
  }

  auto meta = get_string(object, "metadata_file", error, where);
  if (!error.message.empty()) return false;
  if (!meta || meta->empty()) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": missing required field 'metadata_file'");
    return false;
  }
  out.metadata_file = *meta;

  auto media = get_string(object, "media_file", error, where);
  if (!error.message.empty()) return false;
  if (!media || media->empty()) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                       where + ": missing required field 'media_file'");
    return false;
  }
  out.media_file = *media;

  // Preserve unknown track keys for round-tripping.
  static const std::set<std::string, std::less<>> known = {
      "id", "kind", "codec", "rate", "pix_fmt", "width", "height",
      "sample_rate", "channels", "clock", "depends_on", "required_fields",
      "optional_fields", "metadata_file", "media_file"};
  for (const auto& [key, value] : object.members()) {
    if (!known.contains(key)) {
      out.extra.set(key, value);
    }
  }
  return true;
}

bool read_text_file(const fs::path& path, std::string& out, Error& error) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kIo, "read_failed",
                       "cannot read " + path.string() + ": " +
                           std::strerror(errno));
    return false;
  }
  std::string data((std::istreambuf_iterator<char>(in)),
                   std::istreambuf_iterator<char>());
  if (in.bad()) {
    error = make_error(ErrorCategory::kIo, "read_failed",
                       "read error on " + path.string());
    return false;
  }
  out = std::move(data);
  return true;
}

}  // namespace

const std::vector<std::string>& default_required_frame_fields() {
  static const std::vector<std::string> fields = {"seq", "capture_ticks",
                                                  "sha256"};
  return fields;
}

DeliveryLoadResult load_delivery(const std::string& dir) {
  DeliveryLoadResult result;
  const fs::path path = fs::path(dir) / "delivery.json";
  std::string text;
  if (!read_text_file(path, text, result.error)) {
    return result;
  }
  JsonParseResult parsed = parse_json(text);
  if (!parsed.ok) {
    result.error = make_error(ErrorCategory::kInvalidInput, "bad_json",
                              path.string() + ": " + parsed.error);
    set_offset(result.error, parsed.error_at);
    return result;
  }
  if (!parsed.value.is_object()) {
    result.error = make_error(ErrorCategory::kInvalidInput, "bad_delivery",
                              path.string() + ": top level must be an object");
    return result;
  }
  const Json& root = parsed.value;

  auto version = get_int(root, "schema_version", result.error, "delivery");
  if (!result.error.message.empty()) return result;
  if (!version) {
    result.error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                              "delivery: missing required field 'schema_version'");
    return result;
  }
  result.delivery.schema_version = static_cast<int>(*version);
  if (result.delivery.schema_version != kSchemaVersion) {
    result.error = make_error(
        ErrorCategory::kInvalidInput, "unsupported_schema_version",
        "delivery: unsupported schema_version " +
            std::to_string(result.delivery.schema_version) + " (expected " +
            std::to_string(kSchemaVersion) + ")");
    return result;
  }

  if (auto producer = get_string(root, "producer", result.error, "delivery")) {
    result.delivery.producer = *producer;
  } else if (!result.error.message.empty()) {
    return result;
  }
  if (auto created = get_string(root, "created", result.error, "delivery")) {
    result.delivery.created = *created;
  } else if (!result.error.message.empty()) {
    return result;
  }

  const Json* tracks = root.find("tracks");
  if (!tracks || !tracks->is_array()) {
    result.error = make_error(ErrorCategory::kInvalidInput, "missing_field",
                              "delivery: 'tracks' must be an array");
    return result;
  }
  for (std::size_t i = 0; i < tracks->items().size(); ++i) {
    DeliveryTrack track;
    if (!parse_track(tracks->items()[i], i, track, result.error)) {
      return result;
    }
    result.delivery.tracks.push_back(std::move(track));
  }

  static const std::set<std::string, std::less<>> known = {
      "schema_version", "producer", "created", "tracks"};
  for (const auto& [key, value] : root.members()) {
    if (!known.contains(key)) {
      result.delivery.extra.set(key, value);
    }
  }

  result.ok = true;
  return result;
}

bool resolve_delivery_path(const std::string& dir, const std::string& relative,
                           std::string& absolute, Error& error) {
  const fs::path base = fs::weakly_canonical(fs::path(dir));
  fs::path candidate(relative);
  if (candidate.is_absolute()) {
    error = make_error(ErrorCategory::kInvalidInput, "absolute_path",
                       "delivery member '" + relative +
                           "' must be relative to the delivery directory");
    return false;
  }
  fs::path joined = base / candidate;
  std::error_code ec;
  fs::path normalized = fs::weakly_canonical(joined, ec);
  if (ec) {
    error = make_error(ErrorCategory::kInvalidInput, "bad_path",
                       "cannot resolve delivery member '" + relative + "': " +
                           ec.message());
    return false;
  }
  // Containment check on the resolved path: reject "../" escapes and symlinks
  // pointing outside the delivery tree.
  auto base_it = base.begin();
  auto path_it = normalized.begin();
  bool inside = true;
  for (; base_it != base.end(); ++base_it, ++path_it) {
    if (path_it == normalized.end() || *base_it != *path_it) {
      inside = false;
      break;
    }
  }
  if (!inside) {
    error = make_error(ErrorCategory::kInvalidInput, "path_escape",
                       "delivery member '" + relative +
                           "' escapes the delivery directory");
    return false;
  }
  absolute = normalized.string();
  return true;
}

FrameParseResult parse_frame_metadata(std::string_view line,
                                      std::uint64_t line_no,
                                      std::uint64_t offset,
                                      const TrackDesc& track) {
  FrameParseResult result;
  result.error.code = "frame_record";
  auto fail = [&](ErrorCategory category, std::string code,
                  std::string message) {
    result.error = make_error(category, std::move(code), std::move(message));
    result.error.track = track.id;
    set_offset(result.error, offset);
    result.ok = false;
    return result;
  };

  JsonParseResult parsed = parse_json(line);
  if (!parsed.ok) {
    return fail(ErrorCategory::kInvalidInput, "bad_frame_json",
                "line " + std::to_string(line_no) + ": " + parsed.error);
  }
  const Json& object = parsed.value;
  if (!object.is_object()) {
    return fail(ErrorCategory::kInvalidInput, "bad_frame_record",
                "line " + std::to_string(line_no) + ": record must be an object");
  }

  // Required fields: schema defaults plus anything the track adds.  A field
  // the schema marks required but omits is invalid input, not corruption.
  std::vector<std::string> required = default_required_frame_fields();
  for (const std::string& field : track.required_fields) {
    required.push_back(field);
  }
  for (const std::string& field : required) {
    if (!object.find(field)) {
      return fail(ErrorCategory::kInvalidInput, "missing_field",
                  "line " + std::to_string(line_no) + ": missing required field '" +
                      field + "'");
    }
  }

  FrameMetadata frame;
  frame.line = line_no;
  frame.offset = offset;

  auto seq = get_int(object, "seq", result.error, "frame");
  if (!result.error.message.empty()) {
    result.error.track = track.id;
    set_offset(result.error, offset);
    return result;
  }
  if (*seq < 0) {
    return fail(ErrorCategory::kInvalidInput, "bad_seq",
                "line " + std::to_string(line_no) + ": seq must be non-negative");
  }
  frame.seq = static_cast<std::uint64_t>(*seq);

  const Json* capture = object.find("capture_ticks");
  if (capture && !capture->is_null()) {
    if (!capture->is_int()) {
      return fail(ErrorCategory::kInvalidInput, "field_type",
                  "line " + std::to_string(line_no) +
                      ": capture_ticks must be an integer or null");
    }
    frame.has_capture = true;
    frame.capture_ticks = capture->as_int();
  }

  // keyframe accepts a JSON boolean or 0/1 integer; both appear in the wild.
  const Json* keyframe = object.find("keyframe");
  if (keyframe) {
    if (keyframe->is_bool()) {
      frame.keyframe = keyframe->as_bool();
    } else if (keyframe->is_int()) {
      frame.keyframe = keyframe->as_int() != 0;
    } else {
      return fail(ErrorCategory::kInvalidInput, "field_type",
                  "line " + std::to_string(line_no) +
                      ": keyframe must be a boolean or 0/1");
    }
  }

  const Json* pix = object.find("pix_fmt");
  if (pix) {
    if (!pix->is_string()) {
      return fail(ErrorCategory::kInvalidInput, "field_type",
                  "line " + std::to_string(line_no) + ": pix_fmt must be a string");
    }
    auto parsed_pix = parse_pixel_format(pix->as_string());
    if (!parsed_pix) {
      return fail(ErrorCategory::kInvalidInput, "bad_pixel_format",
                  "line " + std::to_string(line_no) +
                      ": unsupported pixel format '" + pix->as_string() + "'");
    }
    frame.pix_fmt = *parsed_pix;
  } else if (!track.pix_fmt.empty()) {
    frame.pix_fmt = parse_pixel_format(track.pix_fmt);
  }

  auto width = get_int(object, "width", result.error, "frame");
  if (!result.error.message.empty()) {
    result.error.track = track.id;
    set_offset(result.error, offset);
    return result;
  }
  auto height = get_int(object, "height", result.error, "frame");
  if (!result.error.message.empty()) {
    result.error.track = track.id;
    set_offset(result.error, offset);
    return result;
  }
  frame.width = width ? static_cast<int>(*width) : track.width;
  frame.height = height ? static_cast<int>(*height) : track.height;
  if (frame.width < 0 || frame.height < 0) {
    return fail(ErrorCategory::kInvalidInput, "bad_dimensions",
                "line " + std::to_string(line_no) + ": negative dimensions");
  }

  const Json* sha = object.find("sha256");
  if (!sha || !sha->is_string()) {
    return fail(ErrorCategory::kInvalidInput, "field_type",
                "line " + std::to_string(line_no) + ": sha256 must be a string");
  }
  auto digest = parse_hex_digest(sha->as_string());
  if (!digest) {
    return fail(ErrorCategory::kInvalidInput, "bad_checksum",
                "line " + std::to_string(line_no) +
                    ": sha256 must be 64 hexadecimal digits");
  }
  frame.sha = *digest;

  // Unknown fields are retained so optional shooting metadata survives.
  static const std::set<std::string, std::less<>> known = {
      "seq", "capture_ticks", "keyframe", "pix_fmt", "width", "height",
      "sha256"};
  Json attrs = Json::make_object();
  for (const auto& [key, value] : object.members()) {
    if (!known.contains(key)) {
      attrs.set(key, value);
    }
  }
  frame.attrs = std::move(attrs);

  result.frame = std::move(frame);
  result.ok = true;
  return result;
}

bool load_frame_metadata(const std::string& path, const TrackDesc& track,
                         std::vector<FrameMetadata>& out, Error& error) {
  error = Error{};
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kIo, "read_failed",
                       "cannot read " + path + ": " + std::strerror(errno));
    return false;
  }
  std::string content((std::istreambuf_iterator<char>(in)),
                      std::istreambuf_iterator<char>());
  if (in.bad()) {
    error = make_error(ErrorCategory::kIo, "read_failed",
                       "read error on " + path);
    return false;
  }

  std::uint64_t line_no = 0;
  std::size_t pos = 0;
  while (pos < content.size()) {
    std::size_t end = content.find('\n', pos);
    const bool truncated = end == std::string::npos;
    if (truncated) {
      end = content.size();
    }
    std::string_view line(content.data() + pos, end - pos);
    const std::uint64_t offset = pos;
    const std::uint64_t this_line = ++line_no;
    if (!line.empty() && line.back() == '\r') {
      line.remove_suffix(1);
    }
    if (!line.empty()) {
      FrameParseResult parsed =
          parse_frame_metadata(line, this_line, offset, track);
      if (!parsed.ok) {
        error = std::move(parsed.error);
        return false;
      }
      out.push_back(std::move(parsed.frame));
    }
    if (truncated) {
      // A final line without a trailing newline is complete as long as it
      // parsed; a truncated record fails in parse_frame_metadata above.
      break;
    }
    pos = end + 1;
  }
  return true;
}

}  // namespace mediaqc

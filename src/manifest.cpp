#include "media/manifest.hpp"

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

Json repair_to_json(const RepairRecord& repair) {
  Json value = Json::make_object();
  value.set("track", Json::make_string(repair.track));
  value.set("seq", Json::make_int(static_cast<std::int64_t>(repair.seq)));
  value.set("offset", Json::make_int(static_cast<std::int64_t>(repair.record_offset)));
  value.set("raw_ns", Json::make_int(repair.raw_ns));
  value.set("fixed_ns", Json::make_int(repair.fixed_ns));
  Json reasons = Json::make_array();
  for (const std::string& name : repair_reason_names(repair.reasons)) {
    reasons.push(Json::make_string(name));
  }
  value.set("reasons", std::move(reasons));
  return value;
}

RepairRecord repair_from_json(const Json& value) {
  RepairRecord repair;
  if (const Json* v = value.find("track")) repair.track = v->as_string();
  if (const Json* v = value.find("seq")) repair.seq = static_cast<std::uint64_t>(v->as_int());
  if (const Json* v = value.find("offset")) repair.record_offset = static_cast<std::uint64_t>(v->as_int());
  if (const Json* v = value.find("raw_ns")) repair.raw_ns = v->as_int();
  if (const Json* v = value.find("fixed_ns")) repair.fixed_ns = v->as_int();
  if (const Json* v = value.find("reasons"); v && v->is_array()) {
    for (const Json& name : v->items()) {
      const std::string& text = name.as_string();
      if (text == "missing") repair.reasons |= static_cast<RepairMask>(RepairReason::kMissing);
      else if (text == "non_monotonic") repair.reasons |= static_cast<RepairMask>(RepairReason::kNonMonotonic);
      else if (text == "wraparound") repair.reasons |= static_cast<RepairMask>(RepairReason::kWraparound);
      else if (text == "drift") repair.reasons |= static_cast<RepairMask>(RepairReason::kDrift);
    }
  }
  return repair;
}

}  // namespace

Json Manifest::to_json() const {
  Json root = Json::make_object();
  root.set("version", Json::make_int(version));
  root.set("frames_total", Json::make_int(static_cast<std::int64_t>(frames_total)));
  root.set("frames_repaired", Json::make_int(static_cast<std::int64_t>(frames_repaired)));
  root.set("frames_verified", Json::make_int(static_cast<std::int64_t>(frames_verified)));
  root.set("frames_corrupt", Json::make_int(static_cast<std::int64_t>(frames_corrupt)));
  root.set("frames_duplicate", Json::make_int(static_cast<std::int64_t>(frames_duplicate)));
  root.set("frames_truncated", Json::make_int(static_cast<std::int64_t>(frames_truncated)));
  root.set("first_corrupt_offset", Json::make_int(first_corrupt_offset));
  root.set("peak_queue_frames", Json::make_int(static_cast<std::int64_t>(peak_queue_frames)));
  root.set("index_entries", Json::make_int(static_cast<std::int64_t>(index_entries)));
  root.set("resume_ms", Json::make_int(resume_ms));
  root.set("resumed", Json::make_bool(resumed));
  root.set("input_sha256", Json::make_string(input_sha256));
  root.set("output_sha256", Json::make_string(output_sha256));
  root.set("index_sha256", Json::make_string(index_sha256));
  root.set("duration_ns", Json::make_int(duration_ns));

  Json track_array = Json::make_array();
  for (const ManifestTrack& track : tracks) {
    Json item = Json::make_object();
    item.set("id", Json::make_string(track.id));
    item.set("kind", Json::make_string(std::string(to_string(track.kind))));
    item.set("codec", Json::make_string(track.codec));
    item.set("frames", Json::make_int(static_cast<std::int64_t>(track.frames)));
    item.set("repaired", Json::make_int(static_cast<std::int64_t>(track.repaired)));
    item.set("dropped", Json::make_int(static_cast<std::int64_t>(track.dropped)));
    Json rate = Json::make_object();
    rate.set("num", Json::make_int(track.rate.num));
    rate.set("den", Json::make_int(track.rate.den));
    item.set("rate", std::move(rate));
    track_array.push(std::move(item));
  }
  root.set("tracks", std::move(track_array));

  Json repair_array = Json::make_array();
  for (const RepairRecord& repair : repairs) {
    repair_array.push(repair_to_json(repair));
  }
  root.set("repairs", std::move(repair_array));
  root.set("errors", errors);
  return root;
}

bool Manifest::from_json(const Json& value, Manifest& out, Error& error) {
  if (!value.is_object()) {
    error = make_error(ErrorCategory::kInvalidInput, "manifest_invalid",
                       "manifest is not an object");
    return false;
  }
  Manifest manifest;
  if (const Json* v = value.find("version")) manifest.version = static_cast<int>(v->as_int());
  if (manifest.version != 1) {
    error = make_error(ErrorCategory::kInvalidInput, "manifest_version",
                       "unsupported manifest version");
    return false;
  }
  auto read_int = [&](const char* key, std::uint64_t& field) {
    if (const Json* v = value.find(key)) field = static_cast<std::uint64_t>(v->as_int());
  };
  read_int("frames_total", manifest.frames_total);
  read_int("frames_repaired", manifest.frames_repaired);
  read_int("frames_verified", manifest.frames_verified);
  read_int("frames_corrupt", manifest.frames_corrupt);
  read_int("frames_duplicate", manifest.frames_duplicate);
  read_int("frames_truncated", manifest.frames_truncated);
  read_int("peak_queue_frames", manifest.peak_queue_frames);
  read_int("index_entries", manifest.index_entries);
  if (const Json* v = value.find("first_corrupt_offset")) manifest.first_corrupt_offset = v->as_int();
  if (const Json* v = value.find("resume_ms")) manifest.resume_ms = v->as_int();
  if (const Json* v = value.find("resumed")) manifest.resumed = v->as_bool();
  if (const Json* v = value.find("duration_ns")) manifest.duration_ns = v->as_int();
  if (const Json* v = value.find("input_sha256")) manifest.input_sha256 = v->as_string();
  if (const Json* v = value.find("output_sha256")) manifest.output_sha256 = v->as_string();
  if (const Json* v = value.find("index_sha256")) manifest.index_sha256 = v->as_string();

  if (const Json* tracks = value.find("tracks"); tracks && tracks->is_array()) {
    for (const Json& item : tracks->items()) {
      ManifestTrack track;
      if (const Json* v = item.find("id")) track.id = v->as_string();
      if (const Json* v = item.find("kind")) {
        if (auto parsed = parse_track_kind(v->as_string())) track.kind = *parsed;
      }
      if (const Json* v = item.find("codec")) track.codec = v->as_string();
      if (const Json* v = item.find("frames")) track.frames = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("repaired")) track.repaired = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("dropped")) track.dropped = static_cast<std::uint64_t>(v->as_int());
      if (const Json* rate = item.find("rate")) {
        if (const Json* num = rate->find("num")) track.rate.num = static_cast<std::int32_t>(num->as_int());
        if (const Json* den = rate->find("den")) track.rate.den = static_cast<std::int32_t>(den->as_int());
      }
      manifest.tracks.push_back(std::move(track));
    }
  }
  if (const Json* repairs = value.find("repairs"); repairs && repairs->is_array()) {
    for (const Json& item : repairs->items()) {
      manifest.repairs.push_back(repair_from_json(item));
    }
  }
  if (const Json* errors = value.find("errors")) manifest.errors = *errors;
  out = std::move(manifest);
  return true;
}

bool write_manifest(const std::string& dir, const Manifest& manifest,
                    Error& error) {
  std::error_code ec;
  fs::create_directories(dir, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "manifest_write_failed",
                       "cannot create " + dir + ": " + ec.message());
    return false;
  }
  const std::string target = (fs::path(dir) / "manifest.json").string();
  const std::string temp = target + ".tmp";
  const std::string text = dump_json(manifest.to_json(), true) + "\n";
  {
    std::ofstream out(temp, std::ios::binary | std::ios::trunc);
    if (!out) {
      error = make_error(ErrorCategory::kIo, "manifest_write_failed",
                         "cannot open " + temp);
      return false;
    }
    out.write(text.data(), static_cast<std::streamsize>(text.size()));
    if (!out) {
      error = make_error(ErrorCategory::kIo, "manifest_write_failed",
                         "cannot write " + temp);
      return false;
    }
  }
  fs::rename(temp, target, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "manifest_write_failed",
                       "cannot publish " + target + ": " + ec.message());
    return false;
  }
  return true;
}

bool load_manifest(const std::string& path, Manifest& out, Error& error) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kInvalidInput, "manifest_missing",
                       "cannot open " + path);
    return false;
  }
  std::string text((std::istreambuf_iterator<char>(in)),
                   std::istreambuf_iterator<char>());
  JsonParseResult parsed = parse_json(text);
  if (!parsed.ok) {
    error = make_error(ErrorCategory::kInvalidInput, "manifest_invalid",
                       path + ": " + parsed.error);
    return false;
  }
  return Manifest::from_json(parsed.value, out, error);
}

}  // namespace mediaqc

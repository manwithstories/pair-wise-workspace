#include "media/checkpoint.hpp"

#include "media/sha256.hpp"

#include <cstdio>
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

Json timeline_to_json(const TimelineState& state) {
  Json value = Json::make_object();
  value.set("started", Json::make_bool(state.started));
  value.set("clock_anchored", Json::make_bool(state.clock_anchored));
  value.set("device_zero_ns", Json::make_int(state.device_zero_ns));
  value.set("last_device_ns", Json::make_int(state.last_device_ns));
  value.set("last_unwrapped_ticks", Json::make_int(state.last_unwrapped_ticks));
  value.set("last_raw_ticks", Json::make_int(static_cast<std::int64_t>(state.last_raw_ticks)));
  value.set("last_slot", Json::make_int(state.last_slot));
  value.set("last_ns", Json::make_int(state.last_ns));
  value.set("wrap_offset", Json::make_int(static_cast<std::int64_t>(state.wrap_offset)));
  value.set("frames", Json::make_int(static_cast<std::int64_t>(state.frames)));
  value.set("repaired", Json::make_int(static_cast<std::int64_t>(state.repaired)));
  return value;
}

TimelineState timeline_from_json(const Json& value) {
  TimelineState state;
  if (const Json* v = value.find("started")) state.started = v->as_bool();
  if (const Json* v = value.find("clock_anchored")) state.clock_anchored = v->as_bool();
  if (const Json* v = value.find("device_zero_ns")) state.device_zero_ns = v->as_int();
  if (const Json* v = value.find("last_device_ns")) state.last_device_ns = v->as_int();
  if (const Json* v = value.find("last_unwrapped_ticks")) state.last_unwrapped_ticks = v->as_int();
  if (const Json* v = value.find("last_raw_ticks")) state.last_raw_ticks = static_cast<std::uint64_t>(v->as_int());
  if (const Json* v = value.find("last_slot")) state.last_slot = v->as_int();
  if (const Json* v = value.find("last_ns")) state.last_ns = v->as_int();
  if (const Json* v = value.find("wrap_offset")) state.wrap_offset = static_cast<std::uint64_t>(v->as_int());
  if (const Json* v = value.find("frames")) state.frames = static_cast<std::uint64_t>(v->as_int());
  if (const Json* v = value.find("repaired")) state.repaired = static_cast<std::uint64_t>(v->as_int());
  return state;
}

}  // namespace

Json Checkpoint::to_json() const {
  Json root = Json::make_object();
  root.set("version", Json::make_int(version));
  root.set("input_sha256", Json::make_string(input_sha256));
  root.set("config_sha256", Json::make_string(config_sha256));
  root.set("output_path", Json::make_string(output_path));
  root.set("frames_committed",
           Json::make_int(static_cast<std::int64_t>(frames_committed)));
  root.set("frames_repaired",
           Json::make_int(static_cast<std::int64_t>(frames_repaired)));
  Json consumed = Json::make_array();
  for (std::uint64_t count : records_consumed) {
    consumed.push(Json::make_int(static_cast<std::int64_t>(count)));
  }
  root.set("records_consumed", std::move(consumed));
  root.set("peak_queue_frames",
           Json::make_int(static_cast<std::int64_t>(peak_queue_frames)));
  root.set("phase", Json::make_string(phase));
  Json timelines = Json::make_array();
  for (const TimelineState& state : timeline) {
    timelines.push(timeline_to_json(state));
  }
  root.set("timeline", std::move(timelines));
  Json segment_array = Json::make_array();
  for (const CommittedSegment& segment : segments) {
    Json item = Json::make_object();
    item.set("track", Json::make_string(segment.track));
    item.set("path", Json::make_string(segment.path));
    item.set("sha256", Json::make_string(segment.sha256));
    item.set("first_frame", Json::make_int(static_cast<std::int64_t>(segment.first_frame)));
    item.set("frames", Json::make_int(static_cast<std::int64_t>(segment.frames)));
    item.set("start_ns", Json::make_int(segment.start_ns));
    item.set("end_ns", Json::make_int(segment.end_ns));
    segment_array.push(std::move(item));
  }
  root.set("segments", std::move(segment_array));
  Json entry_array = Json::make_array();
  for (const CommittedEntry& entry : entries) {
    Json item = Json::make_object();
    item.set("time_ns", Json::make_int(entry.time_ns));
    item.set("frame", Json::make_int(static_cast<std::int64_t>(entry.frame)));
    item.set("frame_index", Json::make_int(static_cast<std::int64_t>(entry.frame_index)));
    item.set("track", Json::make_string(entry.track));
    item.set("segment", Json::make_int(static_cast<std::int64_t>(entry.segment)));
    item.set("offset", Json::make_int(static_cast<std::int64_t>(entry.payload_offset)));
    item.set("size", Json::make_int(static_cast<std::int64_t>(entry.payload_size)));
    item.set("keyframe", Json::make_bool(entry.keyframe));
    item.set("sha256", Json::make_string(entry.sha256));
    entry_array.push(std::move(item));
  }
  root.set("entries", std::move(entry_array));
  root.set("frames_corrupt",
           Json::make_int(static_cast<std::int64_t>(frames_corrupt)));
  root.set("frames_duplicate",
           Json::make_int(static_cast<std::int64_t>(frames_duplicate)));
  root.set("frames_truncated",
           Json::make_int(static_cast<std::int64_t>(frames_truncated)));
  Json error_array = Json::make_array();
  for (const ErrorEntry& error : errors) {
    Json item = Json::make_object();
    item.set("code", Json::make_string(error.code));
    item.set("severity", Json::make_string(error.severity));
    item.set("track", Json::make_string(error.track));
    item.set("offset", Json::make_int(static_cast<std::int64_t>(error.offset)));
    item.set("has_offset", Json::make_bool(error.has_offset));
    item.set("seq", Json::make_int(static_cast<std::int64_t>(error.seq)));
    item.set("has_seq", Json::make_bool(error.has_seq));
    item.set("message", Json::make_string(error.message));
    error_array.push(std::move(item));
  }
  root.set("errors", std::move(error_array));
  root.set("extra", extra);
  return root;
}

bool Checkpoint::from_json(const Json& value, Checkpoint& out, Error& error) {
  if (!value.is_object()) {
    error = make_error(ErrorCategory::kRecovery, "checkpoint_invalid",
                       "checkpoint is not an object");
    return false;
  }
  Checkpoint checkpoint;
  auto require_string = [&](const char* key, std::string& field) -> bool {
    const Json* v = value.find(key);
    if (!v || !v->is_string()) {
      error = make_error(ErrorCategory::kRecovery, "checkpoint_invalid",
                         std::string("checkpoint missing string field '") + key +
                             "'");
      return false;
    }
    field = v->as_string();
    return true;
  };

  if (const Json* v = value.find("version")) {
    checkpoint.version = static_cast<int>(v->as_int());
  }
  if (checkpoint.version != 1) {
    error = make_error(ErrorCategory::kRecovery, "checkpoint_version",
                       "unsupported checkpoint version");
    return false;
  }
  if (!require_string("input_sha256", checkpoint.input_sha256) ||
      !require_string("config_sha256", checkpoint.config_sha256) ||
      !require_string("output_path", checkpoint.output_path)) {
    return false;
  }
  if (const Json* v = value.find("frames_committed")) {
    checkpoint.frames_committed = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* v = value.find("frames_repaired")) {
    checkpoint.frames_repaired = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* consumed = value.find("records_consumed");
      consumed && consumed->is_array()) {
    for (const Json& item : consumed->items()) {
      checkpoint.records_consumed.push_back(static_cast<std::uint64_t>(item.as_int()));
    }
  }
  if (const Json* v = value.find("peak_queue_frames")) {
    checkpoint.peak_queue_frames = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* v = value.find("phase")) {
    checkpoint.phase = v->as_string();
  }
  if (const Json* timelines = value.find("timeline"); timelines && timelines->is_array()) {
    for (const Json& item : timelines->items()) {
      checkpoint.timeline.push_back(timeline_from_json(item));
    }
  }
  if (const Json* segments = value.find("segments"); segments && segments->is_array()) {
    for (const Json& item : segments->items()) {
      CommittedSegment segment;
      if (const Json* v = item.find("track")) segment.track = v->as_string();
      if (const Json* v = item.find("path")) segment.path = v->as_string();
      if (const Json* v = item.find("sha256")) segment.sha256 = v->as_string();
      if (const Json* v = item.find("first_frame")) segment.first_frame = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("frames")) segment.frames = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("start_ns")) segment.start_ns = v->as_int();
      if (const Json* v = item.find("end_ns")) segment.end_ns = v->as_int();
      checkpoint.segments.push_back(std::move(segment));
    }
  }
  if (const Json* entries = value.find("entries"); entries && entries->is_array()) {
    for (const Json& item : entries->items()) {
      CommittedEntry entry;
      if (const Json* v = item.find("time_ns")) entry.time_ns = v->as_int();
      if (const Json* v = item.find("frame")) entry.frame = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("frame_index")) entry.frame_index = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("track")) entry.track = v->as_string();
      if (const Json* v = item.find("segment")) entry.segment = static_cast<std::size_t>(v->as_int());
      if (const Json* v = item.find("offset")) entry.payload_offset = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("size")) entry.payload_size = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("keyframe")) entry.keyframe = v->as_bool();
      if (const Json* v = item.find("sha256")) entry.sha256 = v->as_string();
      checkpoint.entries.push_back(std::move(entry));
    }
  }
  if (const Json* v = value.find("frames_corrupt")) {
    checkpoint.frames_corrupt = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* v = value.find("frames_duplicate")) {
    checkpoint.frames_duplicate = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* v = value.find("frames_truncated")) {
    checkpoint.frames_truncated = static_cast<std::uint64_t>(v->as_int());
  }
  if (const Json* errs = value.find("errors"); errs && errs->is_array()) {
    for (const Json& item : errs->items()) {
      ErrorEntry entry;
      if (const Json* v = item.find("code")) entry.code = v->as_string();
      if (const Json* v = item.find("severity")) entry.severity = v->as_string();
      if (const Json* v = item.find("track")) entry.track = v->as_string();
      if (const Json* v = item.find("offset")) entry.offset = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("has_offset")) entry.has_offset = v->as_bool();
      if (const Json* v = item.find("seq")) entry.seq = static_cast<std::uint64_t>(v->as_int());
      if (const Json* v = item.find("has_seq")) entry.has_seq = v->as_bool();
      if (const Json* v = item.find("message")) entry.message = v->as_string();
      checkpoint.errors.push_back(std::move(entry));
    }
  }
  if (const Json* extra = value.find("extra")) {
    checkpoint.extra = *extra;
  }
  out = std::move(checkpoint);
  return true;
}

bool load_checkpoint(const std::string& path, Checkpoint& out, bool& found,
                     Error& error) {
  found = false;
  std::error_code ec;
  if (!fs::exists(path, ec)) {
    return false;
  }
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    error = make_error(ErrorCategory::kRecovery, "checkpoint_unreadable",
                       "cannot open checkpoint " + path);
    return false;
  }
  std::string text((std::istreambuf_iterator<char>(in)),
                   std::istreambuf_iterator<char>());
  JsonParseResult parsed = parse_json(text);
  if (!parsed.ok) {
    error = make_error(ErrorCategory::kRecovery, "checkpoint_corrupt",
                       path + ": " + parsed.error);
    return false;
  }
  if (!Checkpoint::from_json(parsed.value, out, error)) {
    return false;
  }
  found = true;
  return true;
}

bool save_checkpoint(const std::string& path, const Checkpoint& checkpoint,
                     Error& error) {
  const std::string text = dump_json(checkpoint.to_json(), true) + "\n";
  const std::string temp = path + ".tmp";
  {
    std::ofstream out(temp, std::ios::binary | std::ios::trunc);
    if (!out) {
      error = make_error(ErrorCategory::kIo, "checkpoint_write_failed",
                         "cannot open " + temp);
      return false;
    }
    out.write(text.data(), static_cast<std::streamsize>(text.size()));
    out.flush();
    if (!out) {
      error = make_error(ErrorCategory::kIo, "checkpoint_write_failed",
                         "cannot write " + temp);
      return false;
    }
  }
  std::error_code ec;
  fs::rename(temp, path, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "checkpoint_write_failed",
                       "cannot publish " + path + ": " + ec.message());
    return false;
  }
  return true;
}

bool remove_checkpoint(const std::string& path, Error& error) {
  std::error_code ec;
  fs::remove(path, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "checkpoint_remove_failed",
                       "cannot remove " + path + ": " + ec.message());
    return false;
  }
  return true;
}

std::string config_digest(const std::string& input_path,
                          const std::string& output_path,
                          std::uint64_t queue_frames,
                          std::uint64_t queue_bytes,
                          std::uint64_t segment_frames) {
  std::string material = "v2\n";
  material += input_path;
  material += '\n';
  material += output_path;
  material += '\n';
  material += std::to_string(queue_frames);
  material += '\n';
  material += std::to_string(queue_bytes);
  material += '\n';
  material += std::to_string(segment_frames);
  return to_hex(sha256(material));
}

}  // namespace mediaqc

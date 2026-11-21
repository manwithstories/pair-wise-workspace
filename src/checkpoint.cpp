#include "media/checkpoint.hpp"

#include "media/file_util.hpp"

namespace media {
namespace {

json::Value paramsToJson(const CheckpointParams& p) {
  json::Value o = json::Value::makeObject();
  o.set("input_path", json::Value::makeString(p.inputPath));
  o.set("output_path", json::Value::makeString(p.outputPath));
  o.set("input_digest", json::Value::makeNumber(static_cast<int64_t>(p.inputDigest)));
  o.set("input_file_size", json::Value::makeNumber(static_cast<int64_t>(p.inputFileSize)));
  o.set("sample_rate", json::Value::makeNumber(p.sampleRate));
  o.set("channels", json::Value::makeNumber(p.channels));
  o.set("bits_per_sample", json::Value::makeNumber(p.bitsPerSample));
  o.set("total_frames", json::Value::makeNumber(p.totalFrames));
  o.set("target_bitrate", json::Value::makeNumber(p.targetBitrate));
  o.set("target_sample_rate", json::Value::makeNumber(p.targetSampleRate));
  o.set("target_channels", json::Value::makeNumber(p.targetChannels));
  o.set("segment_max_samples", json::Value::makeNumber(p.segmentMaxSamples));
  o.set("silence_threshold", json::Value::makeNumber(p.silenceThreshold));
  return o;
}

void paramsFromJson(const json::Value& o, CheckpointParams* p) {
  p->inputPath = o.getString("input_path", "");
  p->outputPath = o.getString("output_path", "");
  p->inputDigest = o.getNumber("input_digest", 0);
  p->inputFileSize = o.getNumber("input_file_size", 0);
  p->sampleRate = static_cast<uint32_t>(o.getNumber("sample_rate", 0));
  p->channels = static_cast<uint16_t>(o.getNumber("channels", 0));
  p->bitsPerSample = static_cast<uint16_t>(o.getNumber("bits_per_sample", 0));
  p->totalFrames = o.getNumber("total_frames", 0);
  p->targetBitrate = o.getNumber("target_bitrate", 0);
  p->targetSampleRate = static_cast<uint32_t>(o.getNumber("target_sample_rate", 0));
  p->targetChannels = static_cast<uint16_t>(o.getNumber("target_channels", 0));
  p->segmentMaxSamples = o.getNumber("segment_max_samples", 0);
  p->silenceThreshold = o.getNumber("silence_threshold", 0);
}

}  // namespace

CheckpointStore::CheckpointStore(std::string path) : path_(std::move(path)) {}

json::Value CheckpointStore::toJson(const CheckpointState& s) {
  json::Value root = json::Value::makeObject();
  root.set("schema_version", json::Value::makeNumber(s.schemaVersion));
  root.set("params", paramsToJson(s.params));
  root.set("completed_segments",
           json::Value::makeNumber(s.completedSegments));
  json::Value dups = json::Value::makeArray();
  for (int id : s.duplicateIds)
    dups.push(json::Value::makeNumber(id));
  root.set("duplicate_ids", dups);
  root.set("transcode_done", json::Value::makeBool(s.transcodeDone));
  root.set("manifest_done", json::Value::makeBool(s.manifestDone));
  root.set("output_bytes", json::Value::makeNumber(static_cast<int64_t>(s.outputBytes)));
  root.set("output_frames", json::Value::makeNumber(s.outputFrames));
  return root;
}

bool CheckpointStore::fromJson(const json::Value& v, CheckpointState* s,
                               std::string* error) {
  if (v.type() != json::Value::Type::Object) {
    if (error) *error = "checkpoint root is not an object";
    return false;
  }
  const json::Value* p = v.find("params");
  if (!p || p->type() != json::Value::Type::Object) {
    if (error) *error = "missing params object";
    return false;
  }
  s->schemaVersion = static_cast<int>(v.getNumber("schema_version", 0));
  if (s->schemaVersion != 1) {
    if (error) *error = "unsupported schema version";
    return false;
  }
  paramsFromJson(*p, &s->params);
  s->completedSegments = static_cast<int>(v.getNumber("completed_segments", 0));
  s->transcodeDone = v.find("transcode_done") &&
                     v.find("transcode_done")->type() == json::Value::Type::Bool &&
                     v.find("transcode_done")->asBool();
  s->manifestDone = v.find("manifest_done") &&
                    v.find("manifest_done")->type() == json::Value::Type::Bool &&
                    v.find("manifest_done")->asBool();
  s->outputBytes = v.getNumber("output_bytes", 0);
  s->outputFrames = v.getNumber("output_frames", 0);
  s->duplicateIds.clear();
  if (const json::Value* d = v.find("duplicate_ids");
      d && d->type() == json::Value::Type::Array) {
    for (const auto& item : d->asArray())
      s->duplicateIds.push_back(static_cast<int>(item.asNumber()));
  }
  return true;
}

MediaError CheckpointStore::save(const CheckpointState& state) const {
  std::string err;
  if (!fsutil::atomicWrite(path_, toJson(state).dump(2), &err))
    return MediaError{ErrorCode::IoFailure, Stage::Checkpoint, path_, -1, -1,
                      err, false};
  return MediaError{};
}

MediaError CheckpointStore::load(CheckpointState* state, bool* exists) const {
  *exists = false;
  if (!fsutil::exists(path_)) return MediaError{};
  std::string text, err;
  if (!fsutil::readAll(path_, &text, &err))
    return MediaError{ErrorCode::CheckpointCorrupt, Stage::Checkpoint, path_,
                      -1, -1, err, false};
  json::Value v = json::parse(text, &err);
  if (!err.empty() || !CheckpointStore::fromJson(v, state, &err))
    return MediaError{ErrorCode::CheckpointCorrupt, Stage::Checkpoint, path_,
                      -1, -1, err, false};
  *exists = true;
  return MediaError{};
}

MediaError CheckpointStore::validate(const CheckpointState& state,
                                     const CheckpointParams& expected) {
  const auto& a = state.params;
  if (a.inputDigest != expected.inputDigest ||
      a.inputFileSize != expected.inputFileSize ||
      a.inputPath != expected.inputPath ||
      a.sampleRate != expected.sampleRate ||
      a.channels != expected.channels ||
      a.bitsPerSample != expected.bitsPerSample ||
      a.totalFrames != expected.totalFrames ||
      a.targetBitrate != expected.targetBitrate ||
      a.targetSampleRate != expected.targetSampleRate ||
      a.targetChannels != expected.targetChannels ||
      a.segmentMaxSamples != expected.segmentMaxSamples ||
      a.silenceThreshold != expected.silenceThreshold) {
    return MediaError{ErrorCode::CheckpointMismatch, Stage::Checkpoint,
                      expected.inputPath, -1, -1,
                      "checkpoint input summary or parameters do not match; "
                      "refusing to resume",
                      false};
  }
  return MediaError{};
}

}  // namespace media

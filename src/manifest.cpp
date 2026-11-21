#include "media/manifest.hpp"

#include "media/file_util.hpp"

namespace media {
namespace {

json::Value rangeJson(const SampleRange& r) {
  json::Value o = json::Value::makeObject();
  o.set("start", json::Value::makeNumber(r.start));
  o.set("end", json::Value::makeNumber(r.end));
  return o;
}

}  // namespace

ManifestWriter::ManifestWriter(std::string path) : path_(std::move(path)) {}

json::Value ManifestWriter::toJson(const Manifest& m) {
  json::Value root = json::Value::makeObject();
  root.set("source_path", json::Value::makeString(m.sourcePath));
  root.set("output_path", json::Value::makeString(m.outputPath));
  root.set("source_sample_rate", json::Value::makeNumber(m.sourceSampleRate));
  root.set("output_sample_rate", json::Value::makeNumber(m.outputSampleRate));
  root.set("source_channels", json::Value::makeNumber(m.sourceChannels));
  root.set("output_channels", json::Value::makeNumber(m.outputChannels));
  root.set("source_digest", json::Value::makeNumber(static_cast<int64_t>(m.sourceDigest)));
  root.set("source_total_frames", json::Value::makeNumber(m.sourceTotalFrames));
  root.set("output_total_frames", json::Value::makeNumber(m.outputTotalFrames));
  root.set("target_bitrate_bps", json::Value::makeNumber(m.targetBitrateBps));
  root.set("actual_bitrate_bps", json::Value::makeNumber(m.actualBitrateBps));
  root.set("alignment_error_samples",
           json::Value::makeNumber(m.alignmentErrorSamples));

  json::Value stats = json::Value::makeObject();
  stats.set("peak", json::Value::makeNumber(m.stats.peak));
  stats.set("rms", json::Value::makeNumber(static_cast<int64_t>(m.stats.rms)));
  stats.set("rms_db", json::Value::makeNumber(static_cast<int64_t>(
                          m.stats.rmsDb() * 1000)));
  stats.set("clipped_samples",
            json::Value::makeNumber(static_cast<int64_t>(m.stats.clippedSamples)));
  stats.set("silent_frames",
            json::Value::makeNumber(static_cast<int64_t>(m.stats.silentFrames)));
  root.set("stats", stats);

  json::Value sil = json::Value::makeArray();
  for (const auto& s : m.silence) {
    json::Value j = json::Value::makeObject();
    j.set("range", rangeJson(s.range));
    j.set("duration_ms", json::Value::makeNumber(s.durationMs));
    sil.push(j);
  }
  root.set("silence", sil);

  json::Value segs = json::Value::makeArray();
  for (const auto& s : m.segments) {
    json::Value j = json::Value::makeObject();
    j.set("id", json::Value::makeNumber(s.id));
    j.set("source_range", rangeJson(s.sourceRange));
    j.set("output_range", rangeJson(s.outputRange));
    j.set("contains_gap", json::Value::makeBool(s.containsGap));
    j.set("duplicate", json::Value::makeBool(s.duplicate));
    j.set("duplicate_of", json::Value::makeNumber(s.duplicateOf));
    j.set("fingerprint", json::Value::makeNumber(static_cast<int64_t>(s.fingerprint)));
    segs.push(j);
  }
  root.set("segments", segs);

  json::Value gaps = json::Value::makeArray();
  for (const auto& g : m.gaps) {
    json::Value j = json::Value::makeObject();
    j.set("bad_offset_bytes", json::Value::makeNumber(g.byteOffset));
    j.set("sample_index", json::Value::makeNumber(g.sampleIndex));
    j.set("code", json::Value::makeString(g.code));
    j.set("message", json::Value::makeString(g.message));
    gaps.push(j);
  }
  root.set("gaps", gaps);
  return root;
}

SampleTick ManifestWriter::verifyAlignment(const Manifest& m) {
  // 输出时间线必须由非重复片段的半开区间首尾相接拼成，且总长一致。
  SampleTick cursor = 0;
  SampleTick sourceCovered = 0;
  for (const auto& s : m.segments) {
    if (s.duplicate) continue;
    if (s.outputRange.start != cursor) return cursor - s.outputRange.start;
    if (s.outputRange.end <= s.outputRange.start) return -1;
    if (s.outputRange.length() != s.sourceRange.length()) return -2;
    cursor = s.outputRange.end;
    sourceCovered += s.sourceRange.length();
  }
  if (cursor != m.outputTotalFrames) return cursor - m.outputTotalFrames;
  // 源时间线：所有片段（含重复）的源区间应恰好覆盖 [0, sourceTotalFrames)。
  if (sourceCovered != m.sourceTotalFrames)
    return sourceCovered - m.sourceTotalFrames;
  return 0;
}

MediaError ManifestWriter::write(const Manifest& m) const {
  std::string err;
  if (!fsutil::atomicWrite(path_, toJson(m).dump(2), &err))
    return MediaError{ErrorCode::IoFailure, Stage::Manifest, path_, -1, -1,
                      err, false};
  return MediaError{};
}

}  // namespace media

#include "media/pipeline.hpp"

#include <algorithm>
#include <set>

#include "media/file_util.hpp"

namespace media {

Pipeline::Pipeline(PipelineConfig cfg) : cfg_(std::move(cfg)) {}

MediaError Pipeline::validatePaths() {
  for (const auto& p : {cfg_.outputPath, cfg_.manifestPath,
                        cfg_.checkpointPath}) {
    if (!fsutil::isWithinDirectory(cfg_.workDir, p))
      return MediaError{ErrorCode::InvalidArgument, Stage::Read, p, -1, -1,
                        "path escapes user-specified work directory", false};
  }
  return MediaError{};
}

CheckpointParams Pipeline::buildParams(const WavSummary& sum) const {
  CheckpointParams p;
  p.inputPath = cfg_.inputPath;
  p.outputPath = cfg_.outputPath;
  p.inputDigest = sum.digest;
  p.inputFileSize = sum.fileSize;
  p.sampleRate = sum.format.sampleRate;
  p.channels = sum.format.channels;
  p.bitsPerSample = sum.format.bitsPerSample;
  p.totalFrames = sum.totalFrames;
  p.targetBitrate = cfg_.targetBitrate;
  p.targetSampleRate = cfg_.targetSampleRate;
  p.targetChannels = cfg_.targetChannels;
  p.segmentMaxSamples = cfg_.maxSegmentSamples;
  p.silenceThreshold = cfg_.silence.threshold;
  return p;
}

std::vector<SampleRange> Pipeline::collectGaps(
    const std::vector<MediaError>& errs) const {
  std::vector<SampleRange> gaps;
  for (const auto& e : errs) {
    SampleTick s = e.sampleIndex >= 0 ? e.sampleIndex : 0;
    gaps.push_back(SampleRange{s, s + 1});
  }
  return gaps;
}

PipelineResult Pipeline::run() {
  PipelineResult r;
  MediaError ve = validatePaths();
  if (!ve.ok()) {
    r.fatal = ve;
    return r;
  }

  // ---- 阶段 1：流式解析 + 静音/统计（第一遍，恒定内存）----
  WavReader reader(cfg_.inputPath, cfg_.corruptionPolicy);
  SilenceDetector detector(cfg_.silence, /*rate 占位后修正*/ 1);
  bool rateSet = false;
  MediaError readErr = reader.read([&](const AudioBlock& b) {
    if (!rateSet) {
      detector = SilenceDetector(cfg_.silence,
                                 reader.summary().format.sampleRate);
      rateSet = true;
    }
    detector.process(b);
    return true;
  });
  if (!readErr.ok()) {
    r.fatal = readErr;
    return r;
  }
  const WavSummary& sum = reader.summary();
  auto silenceRanges = detector.finish(sum.totalFrames);
  r.recoverableErrors = reader.recoverableErrors();
  r.clippedSamples = detector.stats().clippedSamples;
  r.silenceMs = 0;
  for (const auto& s : silenceRanges) r.silenceMs += s.durationMs;
  if (!r.recoverableErrors.empty())
    r.badOffsetBytes = r.recoverableErrors.front().byteOffset;

  // ---- 检查点核对：不匹配拒绝续跑 ----
  CheckpointStore store(cfg_.checkpointPath);
  CheckpointState prior;
  bool cpExists = false;
  MediaError le = store.load(&prior, &cpExists);
  if (!le.ok()) {
    r.fatal = le;
    return r;
  }
  CheckpointParams expected = buildParams(sum);
  if (cpExists) {
    MediaError mismatch = CheckpointStore::validate(prior, expected);
    if (!mismatch.ok()) {
      r.fatal = mismatch;
      return r;
    }
    r.resumed = true;
    r.segmentsBeforeRecovery = prior.completedSegments;
    if (prior.transcodeDone && prior.manifestDone && fsutil::exists(cfg_.outputPath))
      r.status = reader.hadRecoverable() ? ExitStatus::CorruptionRecovered
                                          : ExitStatus::Ok;
  }

  // ---- 阶段 2：切分规划 ----
  SegmenterConfig scfg{cfg_.maxSegmentSamples, cfg_.targetDurationTicks,
                       cfg_.splitOnSilence};
  Segmenter segmenter(scfg, sum.format.sampleRate, silenceRanges,
                      sum.totalFrames, collectGaps(r.recoverableErrors));
  const auto& segments = segmenter.segments();
  r.segmentsCount = static_cast<int>(segments.size());
  r.recoverableSegments = static_cast<int>(
      std::count_if(segments.begin(), segments.end(),
                    [](const Segment& s) { return s.containsGap; }));

  // ---- 阶段 3：第二遍读取，逐段指纹判重 + 转码 ----
  Fingerprinter finger(cfg_.fingerprint);
  std::set<int> duplicateIds;
  std::vector<ManifestSegment> mSegs;
  mSegs.reserve(segments.size());
  for (const auto& s : segments)
    mSegs.push_back(ManifestSegment{s.id, s.range, {}, s.containsGap, false,
                                    -1, 0});

  TranscodeConfig tcfg{cfg_.outputPath, cfg_.targetBitrate,
                       cfg_.targetSampleRate, cfg_.targetChannels};
  Transcoder transcoder(tcfg, sum.format);
  MediaError oe = transcoder.open();
  if (!oe.ok()) {
    r.fatal = oe;
    return r;
  }

  std::vector<int32_t> segSamples;
  SampleTick outCursor = 0;
  MediaError stageErr;
  WavReader second(cfg_.inputPath, CorruptionPolicy::Skip);
  (void)second.read([&](const AudioBlock& block) {
    segmenter.dispatch(block, [&](const Segment& seg, const AudioBlock& sub) {
      segSamples.insert(segSamples.end(), sub.samples,
                        sub.samples + sub.frames * sub.channels);
      bool full = segSamples.size() ==
                  static_cast<size_t>(seg.range.length() * sum.format.channels);
      if (!full) return;
      auto fp = finger.build(seg, segSamples, sum.format.channels);
      DuplicateMatch dm;
      bool dup = finger.findOrRegister(fp, &dm);
      mSegs[seg.id].fingerprint = fp.signature;
      if (dup) {
        duplicateIds.insert(seg.id);
        mSegs[seg.id].duplicate = true;
        mSegs[seg.id].duplicateOf = dm.firstSourceId;
      } else {
        TranscodeInput ti{segSamples.data(),
                          static_cast<size_t>(seg.range.length()),
                          sum.format.channels, sum.format.sampleRate};
        MediaError te = transcoder.push(ti);
        if (!te.ok()) {
          stageErr = te;
          return;
        }
        SampleTick len = seg.range.length();
        mSegs[seg.id].outputRange = SampleRange{outCursor, outCursor + len};
        outCursor += len;
      }
      segSamples.clear();
    });
    return stageErr.ok();
  });
  if (!stageErr.ok()) {
    r.fatal = stageErr;
    return r;
  }

  TranscodeResult tres;
  MediaError fe = transcoder.finish(&tres);
  if (!fe.ok()) {
    r.fatal = fe;
    return r;
  }
  r.targetBitrateBps = tres.targetBitrateBps;
  r.actualBitrateBps = tres.actualBitrateBps;
  r.duplicatesCount = static_cast<int>(duplicateIds.size());

  // ---- 阶段 4：清单与时间线验证 ----
  Manifest manifest;
  manifest.sourcePath = cfg_.inputPath;
  manifest.outputPath = cfg_.outputPath;
  manifest.sourceSampleRate = sum.format.sampleRate;
  manifest.outputSampleRate = tres.outputSampleRate;
  manifest.sourceChannels = sum.format.channels;
  manifest.outputChannels = tres.outputChannels;
  manifest.sourceDigest = sum.digest;
  manifest.sourceTotalFrames = sum.totalFrames;
  manifest.outputTotalFrames = outCursor;
  manifest.targetBitrateBps = tres.targetBitrateBps;
  manifest.actualBitrateBps = tres.actualBitrateBps;
  manifest.stats = detector.stats();
  manifest.silence = silenceRanges;
  manifest.segments = mSegs;
  for (const auto& e : r.recoverableErrors)
    manifest.gaps.push_back(ManifestGap{e.byteOffset, e.sampleIndex,
                                        toString(e.code), e.message});
  manifest.alignmentErrorSamples = ManifestWriter::verifyAlignment(manifest);
  r.alignmentErrorSamples = manifest.alignmentErrorSamples;
  if (r.alignmentErrorSamples != 0) {
    r.fatal = MediaError{ErrorCode::ValidationFailed, Stage::Verify,
                         cfg_.manifestPath, -1, -1,
                         "manifest timeline alignment error", false};
    return r;
  }
  MediaError mwe = ManifestWriter(cfg_.manifestPath).write(manifest);
  if (!mwe.ok()) {
    r.fatal = mwe;
    return r;
  }

  // ---- 阶段 5：检查点原子落盘（幂等恢复依据）----
  CheckpointState state;
  state.params = expected;
  state.completedSegments = r.segmentsCount;
  state.duplicateIds = std::vector<int>(duplicateIds.begin(), duplicateIds.end());
  state.transcodeDone = true;
  state.manifestDone = true;
  state.outputBytes = tres.outputBytes;
  state.outputFrames = tres.outputFrames;
  MediaError cse = store.save(state);
  if (!cse.ok()) {
    r.fatal = cse;
    return r;
  }

  r.status = reader.hadRecoverable() ? ExitStatus::CorruptionRecovered
                                     : ExitStatus::Ok;
  return r;
}

}  // namespace media

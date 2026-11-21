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
  // 缺口锚定在错误发生时的样本序号上，用单点区间表达；切分器按区间相交标记。
  std::vector<SampleRange> gaps;
  for (const auto& e : errs) {
    SampleTick s = e.sampleIndex >= 0 ? e.sampleIndex : 0;
    if (s > 0)
      gaps.push_back(SampleRange{s - 1, s});
    else
      gaps.push_back(SampleRange{0, 1});
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
  SampleTick durationTicks = cfg_.targetDurationTicks;
  if (durationTicks == 0 && cfg_.targetDurationMs > 0)
    durationTicks = msToTicks(cfg_.targetDurationMs,
                              sum.format.sampleRate);
  SegmenterConfig scfg{cfg_.maxSegmentSamples, durationTicks,
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

  // 每段流式指纹状态：仅缓存一个窗口，内存与素材时长无关。
  struct SegState {
    std::unique_ptr<Fingerprinter::StreamingBuilder> hasher;
    SampleTick received = 0;
    SampleTick outStart = 0;
    bool outStartSet = false;
  };
  std::vector<SegState> states(segments.size());
  MediaError stageErr;

  // ---- 阶段 3a：第二遍读取，仅做有界流式指纹与判重，不转码 ----
  WavReader scan(cfg_.inputPath, CorruptionPolicy::Skip);
  (void)scan.read([&](const AudioBlock& block) {
    segmenter.dispatch(block, [&](const Segment& seg, const AudioBlock& sub) {
      SegState& st = states[seg.id];
      if (!st.hasher)
        st.hasher = std::make_unique<Fingerprinter::StreamingBuilder>(
            seg.id, seg.range, cfg_.fingerprint, sum.format.channels);
      st.hasher->add(sub.samples, sub.frames);
      st.received += static_cast<SampleTick>(sub.frames);
      if (st.received >= seg.range.length()) {
        SegmentFingerprint fp = st.hasher->finish();
        mSegs[seg.id].fingerprint = fp.signature;
        DuplicateMatch dm;
        if (finger.findOrRegister(fp, &dm)) {
          duplicateIds.insert(seg.id);
          mSegs[seg.id].duplicate = true;
          mSegs[seg.id].duplicateOf = dm.firstSourceId;
        }
      }
    });
    return true;
  });

  // ---- 阶段 3b：第三遍读取，仅转码非重复段，按块直推编码器 ----
  SampleTick outCursor = 0;
  WavReader third(cfg_.inputPath, CorruptionPolicy::Skip);
  (void)third.read([&](const AudioBlock& block) {
    segmenter.dispatch(block, [&](const Segment& seg, const AudioBlock& sub) {
      if (mSegs[seg.id].duplicate) return;
      SegState& st = states[seg.id];
      if (!st.outStartSet) {
        st.outStart = transcoder.encodedFrames();
        st.outStartSet = true;
      }
      TranscodeInput ti{sub.samples, sub.frames, sub.channels,
                        sum.format.sampleRate};
      MediaError te = transcoder.push(ti);
      if (!te.ok()) {
        stageErr = te;
        return;
      }
      SampleTick after = transcoder.encodedFrames();
      mSegs[seg.id].outputRange.start = st.outStart;
      mSegs[seg.id].outputRange.end = after;
      outCursor = after;
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

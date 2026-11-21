#include "media/pipeline.hpp"

#include "media/frame_reader.hpp"
#include "media/index_writer.hpp"
#include "media/manifest.hpp"
#include "media/sha256.hpp"
#include "media/track_graph.hpp"

#include <algorithm>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <limits>
#include <memory>
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

std::int64_t now_ms() {
  return std::chrono::duration_cast<std::chrono::milliseconds>(
             std::chrono::steady_clock::now().time_since_epoch())
      .count();
}

}  // namespace

Json PipelineReport::to_json() const {
  Json root = Json::make_object();
  root.set("frames_total", Json::make_int(static_cast<std::int64_t>(frames_total)));
  root.set("frames_repaired", Json::make_int(static_cast<std::int64_t>(frames_repaired)));
  root.set("frames_verified", Json::make_int(static_cast<std::int64_t>(frames_verified)));
  root.set("frames_corrupt", Json::make_int(static_cast<std::int64_t>(frames_corrupt)));
  root.set("frames_duplicate", Json::make_int(static_cast<std::int64_t>(frames_duplicate)));
  root.set("frames_truncated", Json::make_int(static_cast<std::int64_t>(frames_truncated)));
  root.set("first_corrupt_offset", Json::make_int(first_corrupt_offset));
  root.set("peak_queue_frames", Json::make_int(static_cast<std::int64_t>(peak_queue_frames)));
  root.set("peak_queue_bytes", Json::make_int(static_cast<std::int64_t>(peak_queue_bytes)));
  root.set("index_entries", Json::make_int(static_cast<std::int64_t>(index_entries)));
  root.set("resume_ms", Json::make_int(resume_ms));
  root.set("resumed", Json::make_bool(resumed));
  root.set("interrupted", Json::make_bool(interrupted));
  root.set("output_sha256", Json::make_string(output_sha256));
  root.set("input_sha256", Json::make_string(input_sha256));
  root.set("errors", errors.to_json());
  return root;
}

Pipeline::Pipeline(PipelineOptions options) : options_(std::move(options)) {}

void Pipeline::cancel() { cancel_requested_.store(true); }

PipelineReport Pipeline::run() {
  PipelineReport report;
  const std::int64_t start_ms = now_ms();

  // ---------------------------------------------------------------- schema
  DeliveryLoadResult delivery = load_delivery(options_.input_dir);
  if (!delivery.ok) {
    report.error = delivery.error;
    return report;
  }
  std::vector<TrackDesc> descs;
  for (const auto& track : delivery.delivery.tracks) descs.push_back(track.desc);
  TrackOrder order;
  OrderError order_error;
  if (!TrackGraph::build(descs, order, order_error)) {
    report.error = order_error.error;
    return report;
  }

  // --------------------------------------------------------------- digests
  Sha256 input_hasher;
  for (const auto& track : delivery.delivery.tracks) {
    std::string meta_path;
    Error path_error;
    if (!resolve_delivery_path(options_.input_dir, track.metadata_file,
                               meta_path, path_error)) {
      report.error = path_error;
      return report;
    }
    std::ifstream in(meta_path, std::ios::binary);
    if (!in) {
      report.error = make_error(ErrorCategory::kIo, "read_failed",
                                "cannot read " + meta_path);
      return report;
    }
    std::string data((std::istreambuf_iterator<char>(in)),
                     std::istreambuf_iterator<char>());
    input_hasher.update(data);
  }
  report.input_sha256 = to_hex(input_hasher.finish());

  // ------------------------------------------------------------ checkpoint
  const std::string config_sha = config_digest(
      options_.input_dir, options_.output_dir, options_.queue_frames,
      options_.queue_bytes, options_.segment_frames);

  Checkpoint checkpoint;
  std::vector<IndexEntry> resumed_entries;
  std::vector<SegmentInfo> resumed_segments;
  bool have_checkpoint = false;
  if (!options_.checkpoint_path.empty() && options_.resume &&
      fs::exists(options_.checkpoint_path)) {
    bool found = false;
    Error checkpoint_error;
    if (!load_checkpoint(options_.checkpoint_path, checkpoint, found,
                         checkpoint_error)) {
      report.error = checkpoint_error;
      return report;
    }
    if (found) {
      if (checkpoint.input_sha256 != report.input_sha256) {
        report.error = make_error(ErrorCategory::kRecovery,
                                  "checkpoint_input_mismatch",
                                  "checkpoint was built from different input");
        return report;
      }
      if (checkpoint.config_sha256 != config_sha) {
        report.error = make_error(ErrorCategory::kRecovery,
                                  "checkpoint_config_mismatch",
                                  "checkpoint configuration changed");
        return report;
      }
      if (checkpoint.phase == "finalized") {
        report.error = make_error(ErrorCategory::kRecovery,
                                  "checkpoint_finalized",
                                  "checkpoint belongs to a completed run");
        return report;
      }
      // Every committed segment must still exist and hash to what was
      // recorded; export only validated output.
      bool valid = true;
      std::size_t slot = 0;
      for (const CommittedSegment& segment : checkpoint.segments) {
        const std::string path = (fs::path(options_.output_dir) / segment.path).string();
        std::string hash_error;
        Sha256Digest digest = sha256_of_file(path, hash_error);
        if (!hash_error.empty() || to_hex(digest) != segment.sha256) {
          valid = false;
          break;
        }
        SegmentInfo info;
        info.track = "";
        info.path = segment.path;
        info.frames = segment.frames;
        info.end_ns = segment.end_ns;
        info.sha256 = segment.sha256;
        std::error_code ec;
        info.bytes = fs::file_size(path, ec);
        resumed_segments.push_back(std::move(info));
        (void)slot;
        ++slot;
      }
      if (!valid) {
        report.error = make_error(ErrorCategory::kRecovery,
                                  "checkpoint_segment_mismatch",
                                  "a committed segment no longer matches the "
                                  "checkpoint digest");
        return report;
      }
      have_checkpoint = true;
      report.resumed = true;
      // Carry forward damage already found in the committed prefix so the
      // resumed run's report is cumulative, not merely what this pass saw.
      for (const ErrorEntry& entry : checkpoint.errors) {
        report.errors.add(entry);
      }
      report.frames_corrupt = checkpoint.frames_corrupt;
      report.frames_duplicate = checkpoint.frames_duplicate;
      report.frames_truncated = checkpoint.frames_truncated;
      report.frames_repaired = checkpoint.frames_repaired;
      report.first_corrupt_offset = report.errors.first_offset();
    }
  }

  // ---------------------------------------------------------------- inputs
  struct TrackStream {
    const DeliveryTrack* track = nullptr;
    std::string media_path;
    std::vector<FrameMetadata> metadata;
    FrameReader reader;
    std::unique_ptr<TrackTimeline> timeline;
    std::unique_ptr<FrameVerifier> verifier;
    std::size_t next = 0;
    bool exhausted = false;
    std::uint64_t repaired = 0;
  };
  std::vector<std::unique_ptr<TrackStream>> streams;
  streams.reserve(delivery.delivery.tracks.size());
  std::vector<std::uint64_t> resume_frames(delivery.delivery.tracks.size(), 0);
  std::vector<std::uint64_t> resume_segment(delivery.delivery.tracks.size(), 0);
  std::uint64_t declared_frames = 0;
  for (std::size_t t = 0; t < delivery.delivery.tracks.size(); ++t) {
    const DeliveryTrack& track = delivery.delivery.tracks[t];
    auto stream = std::make_unique<TrackStream>();
    stream->track = &track;
    stream->timeline = std::make_unique<TrackTimeline>(
        TimelineConfig{track.desc.rate, track.desc.clock});
    stream->verifier = std::make_unique<FrameVerifier>(
        track.desc.id, static_cast<std::uint64_t>(track.desc.width),
        static_cast<std::uint64_t>(track.desc.height));

    std::string meta_path;
    Error path_error;
    if (!resolve_delivery_path(options_.input_dir, track.metadata_file,
                               meta_path, path_error) ||
        !resolve_delivery_path(options_.input_dir, track.media_file,
                               stream->media_path, path_error)) {
      report.error = path_error;
      return report;
    }
    if (!load_frame_metadata(meta_path, track.desc, stream->metadata,
                             path_error)) {
      report.error = path_error;
      return report;
    }
    declared_frames += stream->metadata.size();

    if (have_checkpoint) {
      if (t < checkpoint.timeline.size()) {
        stream->timeline->restore(checkpoint.timeline[t]);
      }
      if (t < checkpoint.records_consumed.size()) {
        resume_frames[t] = std::min<std::uint64_t>(
            checkpoint.records_consumed[t], stream->metadata.size());
        stream->next = static_cast<std::size_t>(resume_frames[t]);
      }
    }
    // Position the reader at the first uncommitted packet.  Packets and
    // metadata are 1:1, so skipping N packets aligns with skipping N records.
    if (!stream->reader.open(stream->media_path, track.desc,
                             FrameReader::Options{}, path_error)) {
      report.error = path_error;
      return report;
    }
    FrameReader::Packet packet;
    bool eof = false;
    for (std::uint64_t skip = 0; skip < resume_frames[t]; ++skip) {
      if (!stream->reader.next(packet, eof, path_error)) {
        break;
      }
    }
    report.frames_verified += resume_frames[t];
    streams.push_back(std::move(stream));
  }
  report.frames_total = declared_frames;

  // --------------------------------------------------------------- writer
  SegmentWriter writer;
  writer.set_segment_frames(options_.segment_frames);
  std::vector<SegmentWriter::TrackSpec> specs;
  for (const auto& track : delivery.delivery.tracks) {
    SegmentWriter::TrackSpec spec;
    spec.desc = track.desc;
    specs.push_back(spec);
  }
  const bool output_has_segments =
      fs::exists(fs::path(options_.output_dir) / "media") &&
      !fs::is_empty(fs::path(options_.output_dir) / "media");
  // A resume keeps committed segments; a forced run clears them.  A fresh run
  // into a directory that already holds segments is refused.
  bool overwrite = options_.force;
  if (!overwrite && !have_checkpoint && output_has_segments) {
    report.error = make_error(ErrorCategory::kInvalidArgument, "output_exists",
                              "output " + options_.output_dir +
                                  " already contains segments (use --force)");
    return report;
  }
  Error writer_error;
  // A forced run clears existing segments; a resume keeps its validated
  // committed segments and continues after them.
  if (!writer.open(options_.output_dir, specs, options_.force, writer_error,
                   have_checkpoint)) {
    report.error = writer_error;
    return report;
  }
  if (have_checkpoint) {
    SegmentWriter::ResumeState resume_state;
    resume_state.next_frame_index.assign(streams.size(), 0);
    resume_state.next_segment_index.assign(streams.size(), 0);
    resume_state.committed_segments.clear();
    for (std::size_t i = 0; i < checkpoint.segments.size(); ++i) {
      const CommittedSegment& committed = checkpoint.segments[i];
      SegmentInfo info;
      info.track = committed.track;
      info.path = committed.path;
      info.first_frame = committed.first_frame;
      info.frames = committed.frames;
      info.start_ns = committed.start_ns;
      info.end_ns = committed.end_ns;
      info.sha256 = committed.sha256;
      std::error_code ec;
      info.bytes = fs::file_size(fs::path(options_.output_dir) / committed.path, ec);
      resume_state.committed_segments.push_back(std::move(info));
    }
    for (const CommittedEntry& committed : checkpoint.entries) {
      IndexEntry entry;
      entry.time_ns = committed.time_ns;
      entry.frame = committed.frame;
      entry.frame_index = committed.frame_index;
      entry.track = committed.track;
      entry.segment = committed.segment;
      entry.payload_offset = committed.payload_offset;
      entry.payload_size = committed.payload_size;
      entry.keyframe = committed.keyframe;
      if (auto digest = parse_hex_digest(committed.sha256)) entry.sha256 = *digest;
      resume_state.committed_entries.push_back(std::move(entry));
    }
    for (std::size_t t = 0; t < streams.size(); ++t) {
      resume_state.next_frame_index[t] = resume_frames[t];
      const std::string& id = delivery.delivery.tracks[t].desc.id;
      std::uint64_t count = 0;
      for (const CommittedSegment& segment : checkpoint.segments) {
        if (segment.track == id) ++count;
      }
      resume_state.next_segment_index[t] = count;
    }
    resume_state.max_segment_slot = checkpoint.segments.size();
    if (!writer.resume(resume_state, writer_error)) {
      report.error = writer_error;
      return report;
    }
  }

  // ------------------------------------------------------------- streaming
  struct Frame {
    std::size_t track = 0;
    std::uint64_t frame_index = 0;
    Ns time_ns = 0;
    std::uint64_t seq = 0;
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha{};
    std::uint64_t size = 0;  // cost charged against the byte budget
    std::vector<std::uint8_t> payload;
  };
  // Bounded queue between the reader and the writer.  The single-threaded
  // design still enforces the cap: the reader refuses to run ahead of the
  // writer by more than `queue_frames` items or `queue_bytes` bytes, which is
  // the backpressure the spec requires.  With a slow consumer the peak stays
  // at the configured cap.
  std::vector<std::unique_ptr<Frame>> pending;
  const std::uint64_t max_pending =
      options_.queue_frames == 0 ? 1 : options_.queue_frames;
  const std::uint64_t max_pending_bytes =
      options_.queue_bytes == 0 ? std::numeric_limits<std::uint64_t>::max()
                                : options_.queue_bytes;
  std::uint64_t pending_bytes = 0;
  std::uint64_t peak_pending = 0;
  std::uint64_t peak_pending_bytes = 0;

  auto flush_oldest = [&]() -> bool {
    if (pending.empty()) return true;
    Frame& frame = *pending.front();
    IndexEntry entry;
    SegmentWriter::FrameInput input;
    input.time_ns = frame.time_ns;
    input.frame = frame.seq;
    input.frame_index = frame.frame_index;
    input.keyframe = frame.keyframe;
    input.sha256 = frame.sha;
    input.payload = std::move(frame.payload);
    Error error;
    if (!writer.write(frame.track, input, entry, error)) {
      report.error = error;
      return false;
    }
    report.index_entries++;
    pending_bytes -= frame.size;
    pending.erase(pending.begin());
    return true;
  };

  bool stop = false;
  std::uint64_t processed = 0;
  while (!stop) {
    if (cancel_requested_.load()) {
      report.interrupted = true;
      stop = true;
      break;
    }
    if (options_.stop_after_frames > 0 && processed >= options_.stop_after_frames) {
      report.interrupted = true;
      stop = true;
      break;
    }
    // Pull one frame from the next active track in round-robin order.
    bool produced = false;
    for (std::size_t t = 0; t < streams.size() && !produced; ++t) {
      TrackStream& stream = *streams[t];
      if (stream.exhausted) continue;
      if (stream.next >= stream.metadata.size()) {
        stream.exhausted = true;
        continue;
      }
      FrameReader::Packet packet;
      bool eof = false;
      Error read_error;
      if (!stream.reader.next(packet, eof, read_error)) {
        if (!eof && !read_error.message.empty()) {
          ErrorEntry entry;
          entry.code = read_error.code;
          entry.track = stream.track->desc.id;
          entry.offset = read_error.offset;
          entry.has_offset = read_error.has_offset;
          entry.message = read_error.message;
          entry.severity = "truncation";
          report.errors.add(std::move(entry));
          ++report.frames_truncated;
          if (report.first_corrupt_offset < 0 && read_error.has_offset) {
            report.first_corrupt_offset = static_cast<std::int64_t>(read_error.offset);
          }
        }
        stream.exhausted = true;
        // A track whose media ended before its metadata is a truncated tail.
        if (stream.next < stream.metadata.size() && report.frames_truncated == 0) {
          ErrorEntry entry;
          entry.code = "truncated_tail";
          entry.track = stream.track->desc.id;
          entry.offset = stream.metadata[stream.next].offset;
          entry.has_offset = true;
          entry.message = "media ended before its metadata records";
          entry.severity = "truncation";
          report.errors.add(std::move(entry));
          ++report.frames_truncated;
          if (report.first_corrupt_offset < 0) {
            report.first_corrupt_offset = static_cast<std::int64_t>(entry.offset);
          }
        }
        continue;
      }

      const FrameMetadata& meta = stream.metadata[stream.next];
      FrameVerdict verdict = stream.verifier->verify(meta, packet.data.data(),
                                                     packet.data.size());
      // Advance the timeline for every frame, in capture order, so later
      // timestamps and repairs account for every record.
      auto mapping = stream.timeline->map(meta.has_capture, meta.capture_ticks);
      if (mapping.reasons != 0) {
        RepairRecord repair;
        repair.track = stream.track->desc.id;
        repair.seq = meta.seq;
        repair.record_offset = meta.offset;
        repair.raw_ns = mapping.raw_ns;
        repair.fixed_ns = mapping.ns;
        repair.reasons = mapping.reasons;
        report.repairs.push_back(std::move(repair));
        ++stream.repaired;
        ++report.frames_repaired;
      }
      ++stream.next;
      ++processed;

      if (verdict.check == FrameCheck::kDamaged) {
        ErrorEntry entry;
        entry.code = "checksum_mismatch";
        entry.track = stream.track->desc.id;
        entry.offset = meta.offset;
        entry.has_offset = true;
        entry.seq = meta.seq;
        entry.has_seq = true;
        entry.frame_index = stream.next - 1;
        entry.has_frame_index = true;
        entry.message = "payload does not match recorded sha256";
        report.errors.add(std::move(entry));
        ++report.frames_corrupt;
        if (report.first_corrupt_offset < 0) {
          report.first_corrupt_offset = static_cast<std::int64_t>(meta.offset);
        }
        produced = true;
        continue;
      }
      if (verdict.check == FrameCheck::kDuplicate) {
        ErrorEntry entry;
        entry.code = "duplicate_seq";
        entry.track = stream.track->desc.id;
        entry.offset = meta.offset;
        entry.has_offset = true;
        entry.seq = meta.seq;
        entry.has_seq = true;
        entry.message = "sequence number repeated; first occurrence kept";
        entry.severity = "order";
        report.errors.add(std::move(entry));
        ++report.frames_duplicate;
        if (report.first_corrupt_offset < 0) {
          report.first_corrupt_offset = static_cast<std::int64_t>(meta.offset);
        }
        produced = true;
        continue;
      }

      ++report.frames_verified;
      // Enforce both queue bounds before admitting another frame: the frame
      // count and the total buffered bytes.  A single frame always fits even
      // if it alone exceeds the byte cap, so the pipeline can make progress.
      const std::uint64_t incoming_bytes = packet.data.size() + 256;
      while (pending.size() >= max_pending ||
             (pending_bytes + incoming_bytes > max_pending_bytes &&
              !pending.empty())) {
        if (!flush_oldest()) {
          return report;
        }
      }
      auto frame = std::make_unique<Frame>();
      frame->track = t;
      frame->frame_index = stream.next - 1;
      frame->time_ns = mapping.ns;
      frame->seq = meta.seq;
      frame->keyframe = packet.keyframe;
      frame->sha = meta.sha;
      frame->size = incoming_bytes;
      frame->payload = std::move(packet.data);
      pending_bytes += incoming_bytes;
      pending.push_back(std::move(frame));
      peak_pending = std::max<std::uint64_t>(peak_pending, pending.size());
      peak_pending_bytes =
          std::max<std::uint64_t>(peak_pending_bytes, pending_bytes);
      produced = true;
    }
    if (!produced) break;  // every track exhausted
  }

  // Drain remaining frames unless we are stopping for an interrupt, in which
  // case the checkpoint records only what was fully written.
  while (!pending.empty()) {
    if (!flush_oldest()) {
      return report;
    }
  }
  report.peak_queue_frames = peak_pending;
  report.peak_queue_bytes = peak_pending_bytes;

  if (report.interrupted) {
    // Finalize the partial segments so everything already written is a valid,
    // independently digestible file.  Then record exactly how far we got.
    MediaIndex partial;
    if (!writer.finish(partial, writer_error)) {
      report.error = writer_error;
      return report;
    }
    Checkpoint saved;
    saved.input_sha256 = report.input_sha256;
    saved.config_sha256 = config_sha;
    saved.output_path = options_.output_dir;
    saved.frames_committed = partial.entries.size();
    saved.frames_repaired = report.frames_repaired;
    saved.frames_corrupt = report.frames_corrupt;
    saved.frames_duplicate = report.frames_duplicate;
    saved.frames_truncated = report.frames_truncated;
    saved.errors = report.errors.entries();
    saved.peak_queue_frames = report.peak_queue_frames;
    saved.phase = "writing";
    for (auto& stream : streams) {
      saved.records_consumed.push_back(stream->next);
      saved.timeline.push_back(stream->timeline->state());
    }
    for (const SegmentInfo& segment : partial.segments) {
      CommittedSegment committed;
      committed.track = segment.track;
      committed.path = segment.path;
      committed.sha256 = segment.sha256;
      committed.first_frame = segment.first_frame;
      committed.frames = segment.frames;
      committed.start_ns = segment.start_ns;
      committed.end_ns = segment.end_ns;
      saved.segments.push_back(std::move(committed));
    }
    for (const IndexEntry& entry : partial.entries) {
      CommittedEntry committed;
      committed.time_ns = entry.time_ns;
      committed.frame = entry.frame;
      committed.frame_index = entry.frame_index;
      committed.track = entry.track;
      committed.segment = entry.segment;
      committed.payload_offset = entry.payload_offset;
      committed.payload_size = entry.payload_size;
      committed.keyframe = entry.keyframe;
      committed.sha256 = to_hex(entry.sha256);
      saved.entries.push_back(std::move(committed));
    }
    if (!options_.checkpoint_path.empty()) {
      Error save_error;
      if (!save_checkpoint(options_.checkpoint_path, saved, save_error)) {
        report.error = save_error;
        return report;
      }
    }
    report.index_entries = partial.entries.size();
    report.ok = true;
    return report;
  }

  MediaIndex index;
  if (!writer.finish(index, writer_error)) {
    report.error = writer_error;
    writer.abort();
    return report;
  }
  index.input_sha256 = report.input_sha256;
  index.frames_repaired = report.frames_repaired;
  index.duration_ns = 0;
  for (const IndexEntry& entry : index.entries) {
    index.duration_ns = std::max(index.duration_ns, entry.time_ns);
  }
  report.index_entries = index.entries.size();

  // Combined output digest: hash segment digests in a stable order.
  Sha256 output_hasher;
  std::vector<SegmentInfo> ordered = index.segments;
  std::sort(ordered.begin(), ordered.end(),
            [](const SegmentInfo& a, const SegmentInfo& b) { return a.path < b.path; });
  for (const SegmentInfo& segment : ordered) {
    output_hasher.update(segment.sha256);
    output_hasher.update("\n");
  }
  report.output_sha256 = to_hex(output_hasher.finish());

  if (!index.write(options_.output_dir, report.error)) {
    return report;
  }

  // Manifest travels with the output for archival verification.
  Manifest manifest;
  manifest.frames_total = report.frames_total;
  manifest.frames_repaired = report.frames_repaired;
  manifest.frames_verified = report.frames_verified;
  manifest.frames_corrupt = report.frames_corrupt;
  manifest.frames_duplicate = report.frames_duplicate;
  manifest.frames_truncated = report.frames_truncated;
  manifest.first_corrupt_offset = report.first_corrupt_offset;
  manifest.peak_queue_frames = report.peak_queue_frames;
  manifest.index_entries = report.index_entries;
  manifest.resume_ms = have_checkpoint ? now_ms() - start_ms : 0;
  manifest.resumed = report.resumed;
  manifest.input_sha256 = report.input_sha256;
  manifest.output_sha256 = report.output_sha256;
  manifest.duration_ns = index.duration_ns;
  manifest.repairs = report.repairs;
  manifest.errors = report.errors.to_json();
  for (std::size_t t = 0; t < delivery.delivery.tracks.size(); ++t) {
    ManifestTrack track;
    track.id = delivery.delivery.tracks[t].desc.id;
    track.kind = delivery.delivery.tracks[t].desc.kind;
    track.codec = delivery.delivery.tracks[t].desc.codec;
    track.rate = delivery.delivery.tracks[t].desc.rate;
    if (t < index.tracks.size()) {
      track.frames = index.tracks[t].frames;
    }
    track.repaired = streams[t]->repaired;
    manifest.tracks.push_back(std::move(track));
  }
  Error manifest_error;
  if (!write_manifest(options_.output_dir, manifest, manifest_error)) {
    report.error = manifest_error;
    return report;
  }

  if (!options_.checkpoint_path.empty()) {
    Error remove_error;
    (void)remove_checkpoint(options_.checkpoint_path, remove_error);
  }

  report.resume_ms = manifest.resume_ms;
  report.ok = true;
  return report;
}

}  // namespace mediaqc

#include "relay/stream_pipeline.hpp"

namespace relay {

StreamPipeline::StreamPipeline(PipelineConfig cfg, std::string out_dir)
    : cfg_(std::move(cfg)),
      writer_(std::move(out_dir), cfg_.segment_frames),
      repairer_(cfg_.clock) {}

bool StreamPipeline::drain_one() {
  if (queue_.empty()) return true;
  FrameRecord rec = std::move(queue_.front());
  queue_.pop_front();

  // Stage 2: checksum verification; a mismatch isolates only this frame.
  if (!checksum_matches(rec)) {
    ++stats_.checksum_isolated;
    return true;
  }
  // Stage 3: FFmpeg decode validation.
  DecodedFrameInfo info = decode_validate(rec.payload);
  if (!info.ok) {
    ++stats_.decode_failed;
    return true;
  }
  // Stage 4: clock repair; flagged frames keep their original values.
  RepairedClock clock = repairer_.observe(rec.node_clock, rec.time_base);
  if (clock.error != ClockError::None) ++stats_.clock_errors;

  // Stage 5: segment write (bounded by frames_per_segment).
  const uint32_t seg = writer_.current_segment_id();
  uint64_t off = writer_.write_frame(rec);
  if (off == UINT64_MAX) return false;
  ++stats_.frames_written;

  // Stage 6: index the repaired frame (only sealed segments are ever
  // resumed, so an incomplete segment never survives into a later run).
  index_.add(rec, clock, seg, off);
  return true;
}

PipelineStatus StreamPipeline::run(const std::string& input_path) {
  if (!writer_.recover()) return PipelineStatus::WriteError;

  FrameContainerReader reader(input_path);
  ParseResult open_res = reader.open();
  if (!open_res.ok()) {
    structural_err_ = open_res;
    return PipelineStatus::MediaError;
  }

  bool eof = false;
  while (!eof) {
    // Producer: fill the bounded queue; a full queue applies backpressure.
    while (queue_.size() < cfg_.queue_capacity && !eof) {
      FrameRecord rec;
      if (reader.next(rec)) {
        ++stats_.input_records;
        queue_.push_back(std::move(rec));
      } else {
        const ParseResult& err = reader.last_error();
        if (err.ok()) {
          eof = true; // clean trailer
        } else if (err.error == ParseError::UnknownTimeBase) {
          ++stats_.parse_rejected; // reject record, keep going
        } else {
          structural_err_ = err; // unrecoverable: report first offset
          while (!queue_.empty()) queue_.pop_front();
          return PipelineStatus::MediaError;
        }
      }
    }
    stats_.peak_buffered_frames =
        std::max(stats_.peak_buffered_frames, queue_.size());
    if (queue_.size() >= cfg_.queue_capacity) {
      ++stats_.backpressure_events; // producer paused until consumer drains
    }
    // Consumer: drain one frame per pass; slower consumers simply make the
    // producer hit the full-queue backpressure path more often.
    if (!queue_.empty()) {
      if (!drain_one()) return PipelineStatus::WriteError;
      if (queue_.size() < cfg_.queue_capacity &&
          stats_.backpressure_events > stats_.resume_events) {
        ++stats_.resume_events;
      }
    } else if (eof) {
      break;
    }
  }
  while (!queue_.empty())
    if (!drain_one()) return PipelineStatus::WriteError;

  if (!writer_.finalize()) return PipelineStatus::WriteError;

  stats_.repaired_wraps = repairer_.repaired_wraps();
  stats_.duplicates = index_.duplicate_count();
  stats_.gaps = index_.gaps().size();
  return PipelineStatus::Ok;
}

} // namespace relay

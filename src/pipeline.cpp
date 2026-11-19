#include "pipeline.hpp"

#include <algorithm>
#include <fstream>
#include <iterator>
#include <utility>

#include "byte_io.hpp"
#include "frame_transform.hpp"
#include "gap_estimator.hpp"
#include "keyframe_index.hpp"
#include "packet_writer.hpp"
#include "sha256.hpp"

namespace mfs {
namespace {

void note_error(RepairResult& result, const RepairError& error) {
  if (result.first_error_offset < 0 ||
      static_cast<int64_t>(error.offset) < result.first_error_offset) {
    result.first_error_offset = static_cast<int64_t>(error.offset);
  }
  switch (error.type) {
    case ErrorType::ChecksumMismatch:
      ++result.checksum_errors;
      break;
    case ErrorType::OutOfWindow:
      ++result.out_of_window_packets;
      break;
    case ErrorType::RecordTruncated:
      ++result.truncated_records;
      break;
    case ErrorType::LimitExceeded:
      result.limit_hit = true;
      break;
    default:
      break;
  }
}

RepairedFrame to_repaired_frame(const Packet& packet) {
  RepairedFrame frame;
  frame.seq = packet.header.frame_seq;
  frame.raw_clock = packet.header.capture_clock;
  frame.clock_missing = packet.header.clock_missing();
  frame.keyframe = packet.header.keyframe();
  frame.width = packet.header.width;
  frame.height = packet.header.height;
  frame.codec = packet.header.codec;
  frame.capture_hz = packet.header.capture_hz;
  frame.source_offset = packet.raw_offset;
  frame.payload_sha256 = packet.checksum;
  frame.time_base = kStreamTimeBase;
  return frame;
}

// Re-encodes every accepted packet and compares the result against the bytes
// that were actually read from the file. Rejected records are not part of the
// check: their payload never entered the pipeline.
void verify_roundtrip(RepairResult& result, std::span<const uint8_t> data,
                      const std::vector<Packet>& by_seq) {
  for (const Packet& packet : by_seq) {
    const std::vector<uint8_t> reencoded = encode_record(packet);
    const uint64_t offset = packet.raw_offset;
    ++result.roundtrip_checked;
    bool match = offset + reencoded.size() <= data.size();
    if (match) {
      match = std::equal(reencoded.begin(), reencoded.end(),
                         data.begin() + static_cast<ptrdiff_t>(offset));
    }
    if (!match) {
      ++result.roundtrip_mismatches;
      result.errors.push_back(
          RepairError{ErrorType::RoundtripMismatch, offset,
                      static_cast<uint32_t>(packet.header.frame_seq),
                      "re-encoded record differs from source bytes"});
      if (result.first_error_offset < 0 ||
          static_cast<int64_t>(offset) < result.first_error_offset) {
        result.first_error_offset = static_cast<int64_t>(offset);
      }
    }
  }
}

// Recomputes each segment digest from the frames it contains and checks it
// against the index entry, then probes the index at every frame timestamp.
void verify_index(RepairResult& result, const KeyframeIndex& index,
                  const std::vector<RepairedFrame>& frames) {
  const std::vector<IndexEntry>& entries = index.entries();
  for (const IndexEntry& entry : entries) {
    Sha256 hasher;
    uint32_t count = 0;
    bool inside = false;
    for (const RepairedFrame& frame : frames) {
      if (frame.seq == entry.keyframe_seq) {
        inside = true;
      }
      const bool next_segment =
          inside && frame.keyframe && frame.seq != entry.keyframe_seq;
      if (next_segment) {
        break;
      }
      if (inside) {
        if (!frame.decoded) {
          continue;
        }
        hasher.update(frame.frame_sha256.data(), frame.frame_sha256.size());
        ++count;
      }
    }
    ++result.segments_hashed;
    if (hasher.finish() == entry.segment_sha256 &&
        count == entry.frame_count) {
      ++result.segment_hash_matches;
    } else {
      result.errors.push_back(
          RepairError{ErrorType::RoundtripMismatch, entry.source_offset,
                      entry.segment_id, "segment digest mismatch"});
    }
  }

  for (const RepairedFrame& frame : frames) {
    if (!frame.decoded) {
      continue;
    }
    ++result.index_queries;
    const SeekResult hit = index.seek(frame.ts_us);
    const bool contains =
        hit.found && frame.ts_us >= hit.segment_start_us &&
        (hit.segment_index + 1 >= entries.size() ||
         frame.ts_us < entries[hit.segment_index + 1].start_ts_us);
    if (contains && hit.time_base == frame.time_base) {
      ++result.index_hits;
    }
  }
}

}  // namespace

std::vector<uint8_t> read_file(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    throw MediaError(ErrorType::FileHeaderTruncated,
                     "cannot open input file: " + path);
  }
  return std::vector<uint8_t>(std::istreambuf_iterator<char>(in),
                              std::istreambuf_iterator<char>());
}

RepairResult run_pipeline(std::span<const uint8_t> data,
                          const PipelineOptions& options) {
  RepairResult result;
  result.input_bytes = data.size();

  // -- Parse and reorder ---------------------------------------------------
  PacketReader reader(data, options.parse);
  ReorderBuffer reorder(options.reorder);

  std::vector<Packet> accepted;  // unique packets in sequence order
  auto absorb = [&](std::vector<Packet> batch) {
    for (Packet& packet : batch) {
      accepted.push_back(std::move(packet));
    }
  };

  Packet packet;
  while (reader.next(packet)) {
    ++result.packets_read;
    absorb(reorder.push(std::move(packet)));
  }
  absorb(reorder.flush());

  result.records_read = reader.records_seen();
  result.truncated_input = reader.truncated();
  for (const RepairError& error : reader.errors()) {
    note_error(result, error);
    result.errors.push_back(error);
  }
  for (const RepairError& error : reorder.errors()) {
    note_error(result, error);
    result.errors.push_back(error);
  }
  result.duplicate_packets = reorder.stats().duplicates;
  result.packets_accepted = accepted.size();

  std::vector<RepairedFrame> frames;
  frames.reserve(accepted.size());
  for (const Packet& source : accepted) {
    frames.push_back(to_repaired_frame(source));
  }

  // -- Timestamp repair ----------------------------------------------------
  TimestampRepair timestamp_repair(options.timestamps);
  result.timestamps = timestamp_repair.repair(frames);
  GapEstimateOptions gap_options;
  gap_options.nominal_interval_us = result.timestamps.nominal_interval_us;
  estimate_lost_ranges(reorder.gaps(), frames, gap_options, result.lost);
  result.lost_ranges = result.lost.size();

  // -- Decode --------------------------------------------------------------
  FrameTransform transform;
  for (size_t i = 0; i < frames.size(); ++i) {
    RepairedFrame& frame = frames[i];
    const Packet& source = accepted[i];
    const DecodeOutcome outcome = transform.decode(
        source.header.codec, source.payload, frame.width, frame.height);
    if (!outcome.ok) {
      ++result.decode_errors;
      result.errors.push_back(
          RepairError{ErrorType::DecodeError, frame.source_offset,
                      static_cast<uint32_t>(frame.seq), outcome.detail});
      if (result.first_error_offset < 0 ||
          static_cast<int64_t>(frame.source_offset) <
              result.first_error_offset) {
        result.first_error_offset = static_cast<int64_t>(frame.source_offset);
      }
      continue;
    }
    frame.decoded = true;
    frame.decoded_width = outcome.width;
    frame.decoded_height = outcome.height;
    frame.frame_sha256 = outcome.content_sha256;
    frame.pts = microseconds_to_ticks(frame.ts_us, frame.time_base);
  }
  result.repaired_frames = frames.size();
  if (frames.empty()) {
    result.empty_sequence = true;
    result.errors.push_back(
        RepairError{ErrorType::EmptySequence, 0, 0, "no frames recovered"});
    result.frames = std::move(frames);
    return result;
  }

  // -- Index ---------------------------------------------------------------
  KeyframeIndex index(options.index_time_base, options.max_segments);
  const bool built = index.build(frames);
  if (index.truncated()) {
    result.errors.push_back(RepairError{ErrorType::SegmentLimit, 0, 0,
                                        "segment budget reached"});
  }
  result.index = index.entries();

  // -- Verification --------------------------------------------------------
  verify_roundtrip(result, data, accepted);
  verify_index(result, index, frames);

  result.roundtrip_match_rate =
      result.roundtrip_checked == 0
          ? 1.0
          : static_cast<double>(result.roundtrip_checked -
                                result.roundtrip_mismatches) /
                static_cast<double>(result.roundtrip_checked);
  result.segment_hash_match_rate =
      result.segments_hashed == 0
          ? (built ? 1.0 : 0.0)
          : static_cast<double>(result.segment_hash_matches) /
                static_cast<double>(result.segments_hashed);
  result.keyframe_index_hit_rate =
      result.index_queries == 0
          ? (built ? 1.0 : 0.0)
          : static_cast<double>(result.index_hits) /
                static_cast<double>(result.index_queries);

  result.frames = std::move(frames);
  return result;
}

bool meets_thresholds(const RepairResult& result,
                      const AcceptanceThresholds& thresholds) {
  return result.roundtrip_match_rate >= thresholds.roundtrip_match_rate &&
         result.keyframe_index_hit_rate >= thresholds.keyframe_index_hit_rate &&
         result.segment_hash_match_rate >= thresholds.segment_hash_match_rate;
}

}  // namespace mfs

#pragma once

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "media/index.hpp"
#include "media/types.hpp"

namespace mediaqc {

// Writes the repaired delivery as self-contained MP4 segments, one stream per
// segment file.  Stream copy is used (never re-encode): each payload keeps the
// exact bytes the supplier signed.  A segment is finalized independently, so
// an interrupted run leaves a set of valid, individually digestible files
// behind and resume only has to append new segments.
class SegmentWriter {
 public:
  struct TrackSpec {
    TrackDesc desc;
  };

  struct FrameInput {
    Ns time_ns = 0;             // repaired timestamp on the unified timeline
    std::uint64_t frame = 0;    // source sequence number
    std::uint64_t frame_index = 0;
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha256{};
    std::vector<std::uint8_t> payload;
  };

  SegmentWriter();
  ~SegmentWriter();
  SegmentWriter(const SegmentWriter&) = delete;
  SegmentWriter& operator=(const SegmentWriter&) = delete;

  // Prepares the output directory layout.  `overwrite` clears existing
  // segment files; otherwise an existing output directory must not already
  // contain segments with the same names.  When `resume_start` is greater
  // than zero, previously committed segments are kept untouched and new
  // segments continue after `resume_segment_index`.
  // Prepares the output directory layout.
  //   overwrite   -- clear any existing segment files (a forced re-run).
  //   allow_existing -- leave existing segment files alone so resume() can
  //                   continue after them.  Mutually exclusive with overwrite.
  // Otherwise an existing output directory with segments is refused.
  bool open(const std::string& output_dir, const std::vector<TrackSpec>& tracks,
            bool overwrite, Error& error,
            bool allow_existing = false);

  // Frames per segment.  Must be set before the first write(); defaults to
  // 500.
  void set_segment_frames(std::uint64_t frames) { segment_frames_ = frames; }

  // Resume support: continue writing after already-committed segments.  Must
  // be called after open() and before the first write().  `resolved` carries
  // the index entries of the committed segments so the final index includes
  // them.
  struct ResumeState {
    std::vector<std::uint64_t> next_frame_index;   // per track
    std::vector<std::uint64_t> next_segment_index;  // per track
    std::vector<SegmentInfo> committed_segments;
    std::vector<IndexEntry> committed_entries;
    std::size_t max_segment_slot = 0;
  };
  bool resume(const ResumeState& state, Error& error);

  // Appends one frame to its track's current segment, rotating the segment
  // once it reaches `segment_frames` frames.  Returns the index entry for the
  // frame; its `segment` field indexes MediaIndex::segments.
  bool write(std::size_t track_index, const FrameInput& frame,
             IndexEntry& entry, Error& error);

  // Finalizes the current segment for every track and fills the segment list.
  bool finish(MediaIndex& index, Error& error);

  // Removes all written segments (used on fatal error or abort).
  void abort();

  [[nodiscard]] std::uint64_t frames_written() const { return frames_written_; }
  [[nodiscard]] const std::vector<TrackSpec>& tracks() const { return tracks_; }
  [[nodiscard]] std::uint64_t segments_written() const { return segments_.size(); }

 private:
  struct TrackState;
  bool open_segment(std::size_t track_index, Error& error);
  bool close_segment(std::size_t track_index, Error& error);

  std::string output_dir_;
  std::vector<TrackSpec> tracks_;
  std::vector<std::unique_ptr<TrackState>> states_;
  std::vector<SegmentInfo> segments_;  // finalized segments this run
  std::vector<SegmentInfo> committed_segments_;  // from a resumed checkpoint
  std::vector<IndexEntry> committed_entries_;     // from a resumed checkpoint
  std::size_t segment_slot_offset_ = 0;  // committed segment count
  std::uint64_t segment_frames_ = 500;  // frames per segment
  std::uint64_t frames_written_ = 0;
  bool opened_ = false;
};

}  // namespace mediaqc

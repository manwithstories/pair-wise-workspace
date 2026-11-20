#include "relay/segmenter.hpp"

#include <algorithm>
#include <cstdlib>
#include <map>
#include <sstream>

#include "relay/hash.hpp"

namespace relay {
namespace {

struct Cut {
  std::size_t frame_index = 0;
  bool keyframe = false;
  std::string note;
};

/// Sequence-origin-aware key lookup: losses are recorded per epoch, and
/// sequence numbers restart inside a new epoch.
bool lost_at(const TimelineTrack& track, int epoch, std::uint32_t sequence) {
  for (const auto& loss : track.losses) {
    if (loss.epoch == epoch && sequence >= loss.first_sequence &&
        sequence < loss.first_sequence + loss.count) {
      return true;
    }
  }
  return false;
}

/// Infers the keyframe cadence from observed keyframes in this track.
std::uint32_t keyframe_cadence(const TimelineTrack& track) {
  std::map<std::uint32_t, std::uint32_t> counts;
  std::uint32_t previous = 0;
  bool have_previous = false;
  for (const auto& frame : track.frames) {
    if (!frame.keyframe) {
      continue;
    }
    if (have_previous && frame.sequence > previous) {
      ++counts[frame.sequence - previous];
    }
    previous = frame.sequence;
    have_previous = true;
  }
  std::uint32_t best = 0;
  std::uint32_t best_count = 0;
  for (const auto& [delta, count] : counts) {
    if (count > best_count) {
      best = delta;
      best_count = count;
    }
  }
  return best;
}

/// True when a delivery gap is large and well-placed enough to have
/// swallowed a keyframe that the cadence predicts.
bool gap_swallows_keyframe(const TimelineTrack& track, int epoch,
                           std::uint32_t begin, std::uint32_t end,
                           std::uint32_t cadence, std::uint32_t& lost_sequence) {
  if (cadence == 0) {
    return false;
  }
  for (const auto& loss : track.losses) {
    if (loss.epoch != epoch || loss.first_sequence >= end ||
        loss.first_sequence + loss.count <= begin) {
      continue;
    }
    for (std::uint32_t q = loss.first_sequence;
         q < loss.first_sequence + loss.count; ++q) {
      if (q % cadence == 0) {
        lost_sequence = q;
        return true;
      }
    }
  }
  return false;
}

}  // namespace

TrackSegments segment_track(const TimelineTrack& track,
                            const SegmentOptions& options, ErrorSink& errors) {
  TrackSegments result;
  result.track = track.id;
  result.timebase = track.timebase;
  if (track.frames.empty()) {
    errors.warn("segment", track.id, 0, "no frames; no segment emitted");
    return result;
  }
  const Micros frame_us =
      std::max<std::int64_t>(1, static_cast<Micros>(track.timebase.frame_duration_us()));
  const Micros allowed = std::min(options.max_boundary_error_us, frame_us);
  const std::uint32_t cadence = keyframe_cadence(track);

  std::vector<Cut> cuts;
  cuts.push_back(Cut{0, track.frames.front().keyframe, {}});
  std::size_t cursor = 0;
  while (cursor + 1 < track.frames.size()) {
    const Micros target =
        track.frames[cursor].pts_us + options.target_duration_us;
    // Nearest frame to the target (used when no keyframe is close enough).
    std::size_t nearest = cursor + 1;
    Micros nearest_error = std::llabs(track.frames[nearest].pts_us - target);
    std::size_t best_key = track.frames.size();
    Micros best_key_error = 0;
    for (std::size_t i = cursor + 1; i < track.frames.size(); ++i) {
      const Micros frame_pts = track.frames[i].pts_us;
      const Micros error = frame_pts - target;
      if (std::llabs(error) < nearest_error) {
        nearest = i;
        nearest_error = std::llabs(error);
      }
      if (frame_pts > target + allowed) {
        break;  // nothing further can be within the boundary tolerance
      }
      if (!track.frames[i].keyframe || std::llabs(error) > allowed) {
        continue;
      }
      if (best_key == track.frames.size() ||
          std::llabs(error) < std::llabs(best_key_error)) {
        best_key = i;
        best_key_error = error;
      }
    }
    if (nearest_error > allowed) {
      break;  // target beyond the last usable boundary
    }
    if (nearest + 1 >= track.frames.size()) {
      // Cutting on the very last frame would leave a one-frame tail; the
      // last segment absorbs it instead.
      break;
    }
    if (best_key != track.frames.size()) {
      cuts.push_back(Cut{best_key, true, {}});
      cursor = best_key;
    } else {
      // No keyframe at the target: cut on the nearest frame (never mid-GOP
      // beyond one frame of error) and flag the segment as unseekable.
      if (nearest <= cursor || nearest + 1 >= track.frames.size()) {
        break;
      }
      cuts.push_back(Cut{nearest, false, "no keyframe within one frame of target"});
      cursor = nearest;
    }
  }

  for (std::size_t c = 0; c < cuts.size(); ++c) {
    const std::size_t begin = cuts[c].frame_index;
    const std::size_t end = c + 1 < cuts.size() ? cuts[c + 1].frame_index
                                                : track.frames.size();
    if (end <= begin) {
      continue;
    }
    Segment segment;
    segment.track = track.id;
    segment.index = static_cast<std::uint64_t>(c);
    segment.start_us = track.frames[begin].pts_us;
    segment.end_us = track.frames[end - 1].pts_us + frame_us;
    segment.start_sequence = track.frames[begin].sequence;
    segment.end_sequence = track.frames[end - 1].sequence;
    segment.frame_count = end - begin;
    segment.status = SegmentStatus::kComplete;

    const int epoch = track.frames[begin].epoch;
    const bool start_is_key = track.frames[begin].keyframe;
    segment.keyframe_sequence = start_is_key ? track.frames[begin].sequence : 0;
    segment.random_access = start_is_key;
    if (!start_is_key) {
      segment.status = SegmentStatus::kUnseekable;
      segment.status_reason =
          "segment starts at non-keyframe sequence " +
          std::to_string(segment.start_sequence) + "; no index is fabricated";
    }

    bool has_gap = false;
    for (std::size_t i = begin; i < end && !has_gap; ++i) {
      const FrameRecord& frame = track.frames[i];
      if (lost_at(track, frame.epoch, frame.sequence)) {
        has_gap = true;
        if (segment.status_reason.empty()) {
          segment.status_reason =
              "delivery gap at sequence " + std::to_string(frame.sequence);
        }
      }
      if (i + 1 < end) {
        const FrameRecord& next = track.frames[i + 1];
        // A non-contiguous step means a delivery gap sits between frames.
        if (next.epoch == frame.epoch && next.sequence > frame.sequence + 1) {
          has_gap = true;
          if (segment.status_reason.empty()) {
            segment.status_reason =
                "delivery gap between sequences " +
                std::to_string(frame.sequence) + " and " +
                std::to_string(next.sequence);
          }
        }
      }
    }
    // Only a gap that falls inside this segment (or right at its start)
    // can invalidate the index for this segment; a later gap makes a later
    // segment unseekable, not this one.
    std::uint32_t lost_key = 0;
    // A gap between the last frame of this segment and the first frame of
    // the next one invalidates the next segment's index, not this one.
    if (gap_swallows_keyframe(track, epoch, segment.start_sequence,
                              segment.end_sequence, cadence, lost_key) ||
        (start_is_key && lost_at(track, epoch, segment.start_sequence) &&
         [&] { return true; }())) {
      segment.random_access = false;
      segment.keyframe_sequence = 0;  // no fabricated index
      segment.status = SegmentStatus::kUnseekable;
      segment.status_reason = "keyframe lost near sequence " +
                              std::to_string(lost_key) +
                              "; affected segment not random-access";
    } else if (has_gap && segment.status != SegmentStatus::kUnseekable) {
      segment.status = SegmentStatus::kGap;
    }
    // An unseekable segment keeps its primary explanation (missing
    // keyframe); the gap note above only fills an empty reason.

    const bool last = c + 1 == cuts.size();
    const Micros duration = segment.end_us - segment.start_us;
    if (last && duration + frame_us < options.target_duration_us) {
      if (segment.status == SegmentStatus::kComplete) {
        segment.status = SegmentStatus::kTailTruncated;
      }
      if (segment.status_reason.empty()) {
        segment.status_reason =
            "tail segment holds " + std::to_string(segment.frame_count) +
            " frame(s), shorter than the " +
            std::to_string(options.target_duration_us / 1000) + " ms target";
      }
    }

    std::ostringstream identity;
    for (std::size_t i = begin; i < end; ++i) {
      const FrameRecord& frame = track.frames[i];
      identity << track.id << '|' << frame.epoch << '|' << frame.sequence
               << '|' << frame.pts_us << '|' << frame.sha256 << '|'
               << frame.source_frame << '\n';
    }
    segment.content_hash = sha256_hex(identity.str());
    if (!segment.random_access) {
      ++result.unseekable;
    }
    result.segments.push_back(std::move(segment));
  }
  return result;
}

}  // namespace relay

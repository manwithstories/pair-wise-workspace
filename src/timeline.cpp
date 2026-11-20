#include "relay/timeline.hpp"

#include <algorithm>
#include <deque>
#include <set>
#include <unordered_map>

namespace relay {
namespace {

Micros epoch_offset(const TrackStream& stream, std::size_t epoch) {
  return stream.epochs[epoch].origin_us;
}

}  // namespace

bool is_usable(const TrackNode& node) {
  return node.state == TrackState::kReady;
}

const TrackNode* find_node(const Timeline& timeline, const TrackId& id) {
  for (const auto& node : timeline.graph) {
    if (node.spec.id == id) {
      return &node;
    }
  }
  return nullptr;
}

std::vector<FrameRecord> global_frames(const Timeline& timeline,
                                       const TimelineTrack& track) {
  (void)timeline;
  // Epoch-local pts already encode a global offset because every epoch's
  // origin is the unwrapped timestamp of its first frame.  Normalise here
  // so consumers never have to know about epoch bookkeeping.
  std::vector<FrameRecord> frames = track.frames;
  std::stable_sort(frames.begin(), frames.end(),
                   [](const FrameRecord& a, const FrameRecord& b) {
                     return a.pts_us < b.pts_us;
                   });
  return frames;
}

Timeline build_timeline(const std::vector<TrackInput>& inputs,
                        const std::vector<TrackStream>& streams,
                        ErrorSink& errors) {
  Timeline timeline;
  std::unordered_map<TrackId, const TrackStream*> stream_by_id;
  std::unordered_map<TrackId, const TrackInput*> input_by_id;
  for (const auto& input : inputs) {
    input_by_id[input.spec.id] = &input;
  }
  for (const auto& stream : streams) {
    stream_by_id[stream.track] = &stream;
  }

  // Kahn's algorithm over declared dependencies; ties keep declaration order.
  std::vector<TrackSpec> specs;
  for (const auto& input : inputs) {
    specs.push_back(input.spec);
  }
  if (!inputs.empty()) {
    timeline.timebase = inputs.front().timebase;
  }
  std::unordered_map<TrackId, std::size_t> index;
  for (std::size_t i = 0; i < specs.size(); ++i) {
    index[specs[i].id] = i;
    if (specs[i].depends_on.empty()) {
      continue;
    }
    for (const auto& dependency : specs[i].depends_on) {
      if (index.count(dependency) == 0 &&
          std::none_of(specs.begin(), specs.end(),
                       [&](const TrackSpec& s) { return s.id == dependency; })) {
        errors.fail("timeline", specs[i].id, 0,
                    "declares unknown dependency " + dependency,
                    specs[i].id + "<-" + dependency);
      }
    }
  }
  std::vector<std::size_t> indegree(specs.size(), 0);
  for (std::size_t i = 0; i < specs.size(); ++i) {
    for (const auto& dependency : specs[i].depends_on) {
      auto it = index.find(dependency);
      if (it != index.end()) {
        ++indegree[i];
      }
    }
  }
  std::deque<std::size_t> ready;
  for (std::size_t i = 0; i < specs.size(); ++i) {
    if (indegree[i] == 0) {
      ready.push_back(i);
    }
  }
  std::vector<std::size_t> order;
  while (!ready.empty()) {
    const std::size_t current = ready.front();
    ready.pop_front();
    order.push_back(current);
    for (std::size_t i = 0; i < specs.size(); ++i) {
      if (std::find(specs[i].depends_on.begin(), specs[i].depends_on.end(),
                    specs[current].id) != specs[i].depends_on.end()) {
        if (--indegree[i] == 0) {
          ready.push_back(i);
        }
      }
    }
  }
  if (order.size() != specs.size()) {
    std::string chain;
    for (const auto& spec : specs) {
      if (std::find(order.begin(), order.end(), index[spec.id]) == order.end()) {
        chain += (chain.empty() ? "" : ",") + spec.id;
      }
    }
    errors.fail("timeline", "", 0,
                "dependency graph has a cycle involving: " + chain, chain);
    return timeline;
  }

  std::unordered_map<TrackId, TrackNode> nodes;
  for (const std::size_t position : order) {
    const TrackSpec& spec = specs[position];
    TrackNode node;
    node.spec = spec;
    const TrackStream* stream = nullptr;
    auto stream_it = stream_by_id.find(spec.id);
    if (stream_it != stream_by_id.end()) {
      stream = stream_it->second;
    }
    const bool empty = stream == nullptr || stream->frame_count() == 0;
    for (const auto& dependency : spec.depends_on) {
      auto dependency_it = nodes.find(dependency);
      if (dependency_it == nodes.end()) {
        node.failed_dependencies.push_back(dependency);
        continue;
      }
      if (!is_usable(dependency_it->second)) {
        node.failed_dependencies.push_back(dependency);
        if (dependency_it->second.state == TrackState::kEmpty) {
          node.empty_dependencies.push_back(dependency);
        }
      }
    }
    if (empty) {
      node.state = TrackState::kEmpty;
    } else if (!node.failed_dependencies.empty()) {
      node.state = TrackState::kBlocked;
    } else {
      node.state = TrackState::kReady;
    }
    if (!node.failed_dependencies.empty() || empty) {
      std::vector<std::string> parts{spec.id};
      std::set<std::string> visited{spec.id};
      std::string next = !node.failed_dependencies.empty()
                             ? node.failed_dependencies.front()
                             : (node.empty_dependencies.empty()
                                    ? std::string{}
                                    : node.empty_dependencies.front());
      while (!next.empty() && visited.insert(next).second) {
        parts.push_back(next);
        auto it = nodes.find(next);
        if (it == nodes.end() || it->second.failed_dependencies.empty()) {
          break;
        }
        next = it->second.failed_dependencies.front();
      }
      for (std::size_t i = 0; i < parts.size(); ++i) {
        if (i != 0) {
          node.dependency_chain += "<-";
        }
        node.dependency_chain += parts[i];
      }
    }
    nodes[spec.id] = node;
    timeline.graph.push_back(node);
  }

  for (auto& node : timeline.graph) {
    if (node.state == TrackState::kEmpty) {
      timeline.empty_tracks.push_back(node.spec.id);
      errors.warn("timeline", node.spec.id, 0,
                  "track is empty; segments and indexes are suppressed");
    } else if (node.state == TrackState::kBlocked) {
      timeline.blocked_tracks.push_back(node.spec.id);
      errors.fail("timeline", node.spec.id, 0,
                  "blocked by dependency chain " + node.dependency_chain,
                  node.dependency_chain);
    }
  }

  bool have_range = false;
  for (const auto& input : inputs) {
    auto node = nodes.find(input.spec.id);
    if (node == nodes.end() || !is_usable(node->second)) {
      continue;
    }
    auto stream_it = stream_by_id.find(input.spec.id);
    if (stream_it == stream_by_id.end()) {
      continue;
    }
    const TrackStream& stream = *stream_it->second;
    TimelineTrack track;
    track.id = input.spec.id;
    track.timebase = input.timebase;
    track.stats = stream.stats;
    track.epochs = static_cast<int>(stream.epochs.size());
    for (const auto& epoch : stream.epochs) {
      const Micros offset = epoch_offset(stream, static_cast<std::size_t>(epoch.index));
      for (const auto& frame : epoch.frames) {
        FrameRecord global = frame;
        global.pts_us += offset;
        global.epoch = epoch.index;
        track.frames.push_back(std::move(global));
        if (global.keyframe) {
          track.keyframe_sequences.push_back(global.sequence);
        }
      }
      track.losses.insert(track.losses.end(), epoch.losses.begin(),
                          epoch.losses.end());
    }
    std::stable_sort(track.frames.begin(), track.frames.end(),
                     [](const FrameRecord& a, const FrameRecord& b) {
                       return a.pts_us < b.pts_us;
                     });
    if (!track.frames.empty()) {
      timeline.start_us = have_range
                              ? std::min(timeline.start_us, track.frames.front().pts_us)
                              : track.frames.front().pts_us;
      timeline.end_us =
          have_range ? std::max(timeline.end_us, track.frames.back().pts_us)
                     : track.frames.back().pts_us;
      have_range = true;
    }
    timeline.tracks.push_back(std::move(track));
  }
  if (!have_range) {
    timeline.start_us = 0;
    timeline.end_us = 0;
  }
  return timeline;
}

}  // namespace relay

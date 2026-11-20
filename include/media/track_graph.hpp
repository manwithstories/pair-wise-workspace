#pragma once
// Track graph: input blocks, output tracks and processing dependencies.
#include <map>
#include <string>
#include <vector>

#include "media/timeline.hpp"

namespace mediaqc {

struct InputBlock {
    std::string id;             // unique block id, e.g. trackA.0001
    std::string track;          // output track this block belongs to
    std::string path;           // source file path
    std::vector<std::string> depends_on;  // block ids processed before this one
    Segment span;               // timeline range covered, on the unified clock
    int sequence = 0;           // ordering within the track
};

struct TrackGraph {
    std::vector<InputBlock> blocks;
    std::vector<std::string> tracks;

    const InputBlock* find(const std::string& id) const;
};

struct GraphReport {
    bool ok = true;
    std::vector<DamageRecord> errors;   // MissingDependency / Cycle / DuplicateSegment
    std::vector<std::string> order;     // topological block order when ok
    // For a rejected cycle, the offending node path (ids joined with " -> ").
    std::string cycle_path;
};

// Validates explicit graph: unknown dependencies, cycles, duplicate or
// overlapping segments on the same track; returns a processing order.
GraphReport validate_graph(const TrackGraph& graph);

}  // namespace mediaqc

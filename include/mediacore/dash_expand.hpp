#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "mediacore/manifest.hpp"

namespace mediacore {

struct TmplCtx {
    std::string media;
    std::string init;
    int64_t timescale = 1;
    int64_t startNumber = 1;
    int64_t duration = 0;
};

// Expanded SegmentTimeline entries as (start tick, duration ticks).
using Timeline = std::vector<std::pair<int64_t, int64_t>>;

// Expand a representation into concrete SegmentRefs, validating each URI
// against the containment root and flagging missing files.
void expandRepresentation(Representation& rep, const TmplCtx& ctx,
                          const Timeline& timeline,
                          const std::filesystem::path& dir,
                          const std::filesystem::path& root,
                          const std::string& trackName, ErrorLog& errors);

}  // namespace mediacore

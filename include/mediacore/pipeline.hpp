#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/json.hpp"
#include "mediacore/manifest.hpp"
#include "mediacore/metadata.hpp"
#include "mediacore/normalize.hpp"
#include "mediacore/probe.hpp"
#include "mediacore/timeline.hpp"

namespace mediacore {

struct AuditOptions {
    std::filesystem::path root;          // containment root
    bool strict = false;                 // stop on first located error
    bool recover = false;                // skip damaged windows
    ColorPolicy colorPolicy = ColorPolicy::Strict;
    std::size_t maxWorkers = 4;
    std::size_t maxPacketsPerSegment = 200'000;
    AudioTarget audioTarget;
};

struct SegmentAudit {
    std::string track;
    int segment = -1;
    std::string uri;
    TimeRange range;
    std::string sha256;                 // empty when unreadable
    bool keyframeIndexHit = false;
    bool keyframeEvaluated = false;
    bool corrupt = false;
    bool skipped = false;                // recovery bypassed this window
};

struct TrackAudit {
    std::string trackId;
    std::string mediaType;
    bool incomplete = false;
    std::vector<SegmentAudit> segments;
    std::vector<StreamInfo> probeStreams;
    std::vector<ResamplePlan> audioPlans;
    ColorReport color;
};

struct AuditMetrics {
    double roundtripConsistency = 0.0;
    double keyframeIndexHitRate = 0.0;
    std::size_t segmentSha256Count = 0;
    std::size_t corruptRanges = 0;
    std::size_t incompleteTracks = 0;
    std::size_t totalSegments = 0;
    std::size_t recoveredSegments = 0;
};

struct AuditReport {
    ManifestFormat format = ManifestFormat::Unknown;
    std::vector<TrackAudit> tracks;
    AuditMetrics metrics;
    std::vector<MediaError> errors;
    bool fatal = false;

    json::Value toJson() const;
    std::string toText() const;
};

// Run the full dependency-ordered pipeline: parse -> timeline -> probe ->
// audio planning -> color checks -> hashing/output verify. In strict mode
// the first fatal error aborts; in recover mode damaged windows are
// skipped and their tracks are marked incomplete.
AuditReport runPipeline(const Manifest& manifest,
                        const std::vector<TrackSpec>& specs,
                        const AuditOptions& opts, ErrorLog& errors);

// Exit codes used by the CLI.
enum class ExitCode : int {
    Ok = 0,
    MediaError = 2,
    ConfigError = 3,
};

}  // namespace mediacore

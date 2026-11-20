#include <catch2/catch_test_macros.hpp>

#include <filesystem>
#include <unistd.h>

#include "mediacore/fixture.hpp"
#include "mediacore/pipeline.hpp"

namespace fs = std::filesystem;
using namespace mediacore;

namespace {

fs::path e2eDir() {
    auto dir = fs::temp_directory_path() /
               ("mcaudit_e2e_" +
                std::to_string(static_cast<long>(::getpid())));
    return dir;
}

AuditOptions baseOptions(const fs::path& root) {
    AuditOptions opts;
    opts.root = root;
    opts.recover = true;
    opts.colorPolicy = ColorPolicy::PreferTag;
    opts.maxWorkers = 2;
    return opts;
}

}  // namespace

TEST_CASE("healthy HLS fixture audits cleanly and deterministically",
          "[pipeline]") {
    auto dir = e2eDir();
    auto layout = fixture::generate(dir);
    ErrorLog errors;
    ParseOptions popts;
    popts.root = dir;
    auto manifest = parseManifest(dir / "master.m3u8", popts, errors);
    auto specs = loadTracks(dir / "tracks.json", errors);
    REQUIRE(manifest.playlists.size() >= 1);

    auto opts = baseOptions(dir);
    auto report = runPipeline(manifest, specs, opts, errors);
    REQUIRE(report.metrics.roundtripConsistency >= 0.99);
    REQUIRE(report.metrics.keyframeIndexHitRate >= 0.95);
    REQUIRE(report.metrics.segmentSha256Count >= 4);

    // Repeated runs produce byte-identical JSON summaries.
    ErrorLog errors2;
    auto m2 = parseManifest(dir / "master.m3u8", popts, errors2);
    auto report2 = runPipeline(m2, specs, opts, errors2);
    REQUIRE(json::dump(report.toJson()) == json::dump(report2.toJson()));
    REQUIRE_FALSE(report.toText().empty());
}

TEST_CASE("broken DASH path locates corruption and marks incompleteness",
          "[pipeline]") {
    auto dir = e2eDir();
    fixture::generate(dir);
    ErrorLog errors;
    ParseOptions popts;
    popts.root = dir;
    auto manifest = parseManifest(dir / "broken.mpd", popts, errors);
    REQUIRE(manifest.format == ManifestFormat::Dash);

    auto opts = baseOptions(dir);
    auto report = runPipeline(manifest, {}, opts, errors);
    // At least one located corrupt/missing range and an incomplete track.
    REQUIRE(report.metrics.corruptRanges >= 1);
    REQUIRE(report.metrics.incompleteTracks >= 1);
    bool located = false;
    for (const auto& e : report.errors) {
        if (e.code == ErrorCode::SegmentCorrupt ||
            e.code == ErrorCode::SegmentMissing)
            located = true;
    }
    REQUIRE(located);
}

TEST_CASE("strict mode surfaces fatal errors with exit semantics",
          "[pipeline]") {
    auto dir = e2eDir();
    fixture::generate(dir);
    ErrorLog errors;
    ParseOptions popts;
    popts.root = dir;
    popts.strict = true;
    auto manifest = parseManifest(dir / "broken.mpd", popts, errors);

    AuditOptions opts = baseOptions(dir);
    opts.strict = true;
    opts.recover = false;
    auto report = runPipeline(manifest, {}, opts, errors);
    // Strict mode never silently recovers: the report carries errors.
    REQUIRE_FALSE(report.errors.empty());
    REQUIRE(static_cast<int>(ExitCode::MediaError) == 2);
    REQUIRE(static_cast<int>(ExitCode::ConfigError) == 3);
    REQUIRE(static_cast<int>(ExitCode::Ok) == 0);
}

TEST_CASE("timeline boundary fixture flags non-divisible mapping",
          "[pipeline]") {
    auto dir = e2eDir();
    fixture::generate(dir);
    ErrorLog errors;
    ParseOptions popts;
    popts.root = dir;
    auto manifest = parseManifest(dir / "boundary.m3u8", popts, errors);
    AuditOptions opts = baseOptions(dir);
    runPipeline(manifest, {}, opts, errors);
    REQUIRE(errors.count(ErrorCode::NonDivisibleScale) >= 1);
}

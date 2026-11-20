#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

#include "mediacore/manifest.hpp"
#include "mediacore/metadata.hpp"
#include "mediacore/pipeline.hpp"

using namespace mediacore;

namespace {

struct CliArgs {
    std::filesystem::path manifest;
    std::filesystem::path tracks;
    bool recover = false;
    bool strict = false;
    bool json = false;
    // Default to retaining stream tags and documenting disagreements;
    // --strict upgrades this to fail-on-conflict.
    ColorPolicy colorPolicy = ColorPolicy::PreferTag;
    std::size_t workers = 4;
    bool showHelp = false;
};

void printHelp() {
    std::cout
        << "usage: media_audit --manifest <file> --tracks <file> [options]\n"
        << "  --recover            skip damaged windows, mark incomplete\n"
        << "  --strict             stop at the first located error\n"
        << "  --json               emit machine-readable JSON summary\n"
        << "  --color-policy P     strict | prefer-tag | prefer-srgb\n"
        << "  --workers N          bounded concurrency (default 4)\n"
        << "  -h, --help           show this help\n";
}

// Returns 0 on success, 3 for a configuration/argument error.
int parseArgs(int argc, char** argv, CliArgs& out, std::string& err) {
    for (int i = 1; i < argc; ++i) {
        const std::string a = argv[i];
        auto need = [&](std::string& dst) -> bool {
            if (i + 1 >= argc) {
                err = "missing value for " + a;
                return false;
            }
            dst = argv[++i];
            return true;
        };
        if (a == "--manifest") {
            if (!need(reinterpret_cast<std::string&>(out.manifest)))
                return 3;
        } else if (a == "--tracks") {
            if (!need(reinterpret_cast<std::string&>(out.tracks)))
                return 3;
        } else if (a == "--recover") {
            out.recover = true;
        } else if (a == "--strict") {
            out.strict = true;
        } else if (a == "--json") {
            out.json = true;
        } else if (a == "--color-policy") {
            std::string v;
            if (!need(v)) return 3;
            out.colorPolicy = parseColorPolicy(v);
        } else if (a == "--workers") {
            std::string v;
            if (!need(v)) return 3;
            try {
                out.workers = static_cast<std::size_t>(std::stoul(v));
            } catch (...) {
                err = "invalid worker count: " + v;
                return 3;
            }
        } else if (a == "-h" || a == "--help") {
            out.showHelp = true;
        } else {
            err = "unknown argument: " + a;
            return 3;
        }
    }
    return 0;
}

}  // namespace

int main(int argc, char** argv) {
    CliArgs cli;
    std::string err;
    if (parseArgs(argc, argv, cli, err) != 0) {
        std::cerr << err << "\n";
        printHelp();
        return static_cast<int>(ExitCode::ConfigError);
    }
    if (cli.showHelp) {
        printHelp();
        return static_cast<int>(ExitCode::Ok);
    }
    if (cli.manifest.empty()) {
        std::cerr << "--manifest is required\n";
        printHelp();
        return static_cast<int>(ExitCode::ConfigError);
    }

    ErrorLog errors;
    ParseOptions popts;
    popts.root = cli.manifest.parent_path();
    popts.strict = cli.strict;
    auto manifest = parseManifest(cli.manifest, popts, errors);
    if (manifest.format == ManifestFormat::Unknown ||
        errors.count(ErrorCode::ManifestInvalid) > 0 ||
        errors.count(ErrorCode::InvalidArgument) > 0) {
        for (const auto& e : errors.items())
            std::cerr << toString(e.code) << ": " << e.detail << "\n";
        return static_cast<int>(ExitCode::ConfigError);
    }

    std::vector<TrackSpec> specs;
    if (!cli.tracks.empty())
        specs = loadTracks(cli.tracks, errors);

    AuditOptions opts;
    opts.root = popts.root;
    opts.strict = cli.strict;
    opts.recover = cli.recover;
    opts.colorPolicy =
        cli.strict ? ColorPolicy::Strict : cli.colorPolicy;
    opts.maxWorkers = cli.workers;

    auto report = runPipeline(manifest, specs, opts, errors);

    if (cli.json)
        std::cout << json::dump(report.toJson());
    else
        std::cout << report.toText();

    // Fatal/configuration problems always win with code 2.
    if (report.fatal || errors.hasFatal())
        return static_cast<int>(ExitCode::MediaError);

    const bool hasMediaError =
        report.metrics.corruptRanges > 0 ||
        report.metrics.incompleteTracks > 0;
    if (!cli.recover && hasMediaError)
        return static_cast<int>(ExitCode::MediaError);
    // In recovery mode damaged windows were skipped and the audit
    // completed, so the run is reported as successful.
    return static_cast<int>(ExitCode::Ok);
}

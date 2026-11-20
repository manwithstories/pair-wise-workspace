#include <algorithm>
#include <filesystem>
#include <iostream>
#include <set>
#include <string>
#include <vector>

#include "media/checkpoint.hpp"
#include "media/fs_util.hpp"
#include "media/manifest.hpp"
#include "media/pipeline.hpp"
#include "media/repack.hpp"
#include "media/timeline.hpp"
#include "media/track_graph.hpp"

namespace {

using namespace mediaqc;

struct Args {
    std::string command;
    std::string input_dir;
    std::string output_dir;
    std::string manifest_path;
    int64_t target_bitrate = 4'608'000;
    int64_t keyframe_gap_ms = 250;
    DamagePolicy policy = DamagePolicy::Silence;
    bool resume = false;
    int target_rate = 48000;
    int target_channels = 6;
};

[[noreturn]] void usage() {
    std::cerr << "usage:\n"
              << "  mediaqc batch --input DIR --output DIR [--target-bitrate N]\n"
              << "                 [--keyframe-gap-ms N] [--damage-policy silence|truncate]\n"
              << "                 [--resume]\n"
              << "  mediaqc verify --manifest FILE\n";
    std::exit(64);
}

DamagePolicy parse_policy(const std::string& s) {
    if (s == "silence") return DamagePolicy::Silence;
    if (s == "truncate") return DamagePolicy::Truncate;
    throw std::runtime_error("unknown damage policy: " + s);
}

Args parse_args(int argc, char** argv) {
    Args a;
    if (argc < 2) usage();
    a.command = argv[1];
    for (int i = 2; i < argc; ++i) {
        const std::string k = argv[i];
        auto next = [&]() -> std::string {
            if (i + 1 >= argc) throw std::runtime_error("missing value for " + k);
            return argv[++i];
        };
        if (k == "--input") a.input_dir = next();
        else if (k == "--output") a.output_dir = next();
        else if (k == "--manifest") a.manifest_path = next();
        else if (k == "--target-bitrate") a.target_bitrate = std::stoll(next());
        else if (k == "--keyframe-gap-ms") a.keyframe_gap_ms = std::stoll(next());
        else if (k == "--damage-policy") a.policy = parse_policy(next());
        else if (k == "--resume") a.resume = true;
        else if (k == "--rate") a.target_rate = std::stoi(next());
        else if (k == "--channels") a.target_channels = std::stoi(next());
        else throw std::runtime_error("unknown argument: " + k);
    }
    return a;
}

struct SourceItem {
    std::string track_id;
    std::string path;
};

std::vector<SourceItem> discover_sources(const std::string& input_dir) {
    std::vector<SourceItem> items;
    for (const auto& name : fs::list_files(input_dir)) {
        if (name.size() < 4 || name.substr(name.size() - 4) != ".wav") continue;
        items.push_back({name.substr(0, name.size() - 4), fs::safe_join(input_dir, name)});
    }
    std::sort(items.begin(), items.end(),
              [](const SourceItem& a, const SourceItem& b) { return a.track_id < b.track_id; });
    return items;
}

int run_batch(const Args& args) {
    if (args.input_dir.empty() || args.output_dir.empty()) usage();
    const std::string in_dir = std::filesystem::canonical(args.input_dir).string();
    const std::string out_dir = fs::validate_output_dir(args.output_dir);
    const auto items = discover_sources(in_dir);
    if (items.empty()) {
        std::cerr << "error: no .wav sources in " << in_dir << "\n";
        return 3;
    }

    // Explicit graph: one block per source; validated for deps, cycles, overlap.
    TrackGraph graph;
    std::set<std::string> track_names;
    for (const auto& it : items) {
        graph.blocks.push_back({it.track_id, it.track_id, it.path, {}, {},
                                static_cast<int>(graph.blocks.size())});
        track_names.insert(it.track_id);
    }
    graph.tracks.assign(track_names.begin(), track_names.end());
    const GraphReport report = validate_graph(graph);
    if (!report.ok) {
        for (const auto& e : report.errors)
            std::cerr << to_string(e.code) << ": " << e.detail << "\n";
        if (!report.cycle_path.empty()) std::cerr << "cycle path: " << report.cycle_path << "\n";
        return 4;
    }

    PipelineOptions opts;
    opts.target_rate_hz = args.target_rate;
    opts.target_channels = args.target_channels;
    opts.target_bitrate = args.target_bitrate;
    opts.keyframe_gap_ms = args.keyframe_gap_ms;
    opts.damage_policy = args.policy;
    opts.resume = args.resume;
    const std::string digest = parameter_digest(opts);

    CheckpointStore cp(out_dir + "/checkpoint.json");
    if (args.resume) cp.load();

    BatchManifest manifest;
    manifest.output_rate_hz = args.target_rate;
    manifest.output_channels = args.target_channels;
    manifest.target_bitrate = args.target_bitrate;

    int64_t total_packets = 0, total_damaged = 0, total_truncated = 0;
    bool any_recoverable = false, any_unrecoverable = false;

    for (const auto& block_id : report.order) {
        const auto* block = graph.find(block_id);
        const auto st = fs::stat(block->path);
        auto maybe_tm = process_track(*block, opts, out_dir, cp, digest, st);
        if (!maybe_tm) { any_unrecoverable = true; continue; }
        auto& tm = *maybe_tm;
        total_packets += tm.packets;
        total_damaged += tm.damaged_ranges;
        total_truncated += tm.truncated_bytes;
        any_recoverable = any_recoverable || !tm.damage.empty();
        manifest.max_alignment_error_ns =
            std::max(manifest.max_alignment_error_ns, tm.alignment_error_ns);
        manifest.tracks.push_back(std::move(tm));
    }

    for (const auto& inv : cp.invalidations())
        std::cout << "checkpoint invalidated: " << inv.track_id << " (" << inv.reason << ")\n";

    if (any_unrecoverable) {
        manifest.unrecoverable = true;
        fs::write_atomic(out_dir + "/failure-summary.json", render_manifest(manifest));
        std::cerr << "failure summary written; no complete manifest generated\n";
        return 5;
    }
    manifest.has_recoverable_damage = any_recoverable;
    fs::write_atomic(out_dir + "/manifest.json", render_manifest(manifest));

    std::cout << "tracks=" << manifest.tracks.size() << " packets=" << total_packets
              << " damaged_ranges=" << total_damaged
              << " truncated_bytes=" << total_truncated << "\n";
    std::cout << "target_bitrate=" << args.target_bitrate
              << " actual_bitrate=" << manifest.tracks.front().actual_bitrate
              << " bitrate_deviation_pct=" << manifest.tracks.front().bitrate_deviation_pct
              << "\n";
    std::cout << "peak_dbfs=" << manifest.tracks.front().peak_dbfs
              << " rms_dbfs=" << manifest.tracks.front().rms_dbfs << "\n";
    std::cout << "manifest_alignment_max_ns=" << manifest.max_alignment_error_ns << "\n";
    return any_recoverable ? 2 : 0;
}

int run_verify(const Args& args) {
    if (args.manifest_path.empty()) usage();
    const BatchManifest m = parse_manifest(args.manifest_path);
    if (m.unrecoverable) {
        std::cerr << "manifest marks batch as unrecoverable\n";
        return 5;
    }
    const SampleClock clk(m.output_rate_hz);
    int64_t max_align = 0, packets = 0, damage = 0, trunc = 0;
    for (const auto& t : m.tracks) {
        if (!fs::stat(t.output_path).exists) {
            std::cerr << "missing output for track " << t.track_id << "\n";
            return 5;
        }
        if (t.alignment_error_ns > clk.period_ns()) {
            std::cerr << "alignment error exceeds one sample period on " << t.track_id << "\n";
            return 5;
        }
        max_align = std::max(max_align, t.alignment_error_ns);
        packets += t.packets;
        damage += t.damaged_ranges;
        trunc += t.truncated_bytes;
    }
    std::cout << "verify ok: tracks=" << m.tracks.size() << " packets=" << packets
              << " damaged_ranges=" << damage << " truncated_bytes=" << trunc
              << " manifest_alignment_max_ns=" << max_align << "\n";
    return m.has_recoverable_damage ? 2 : 0;
}

}  // namespace

int main(int argc, char** argv) try {
    const Args args = parse_args(argc, argv);
    if (args.command == "batch") return run_batch(args);
    if (args.command == "verify") return run_verify(args);
    usage();
} catch (const std::exception& e) {
    std::cerr << "error: " << e.what() << "\n";
    return 6;
}

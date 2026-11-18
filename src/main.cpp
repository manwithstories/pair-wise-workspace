#include <array>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <numeric>
#include <stdexcept>
#include <string>
#include <vector>

#include "allocator.hpp"
#include "diff.hpp"
#include "fixture.hpp"
#include "ppm.hpp"
#include "report.hpp"
#include "segments.hpp"
#include "sequence.hpp"

namespace {

constexpr int kExitOk = 0;
constexpr int kExitParam = 2;
constexpr int kExitInput = 3;
constexpr int kExitWrite = 4;
constexpr int kExitValidate = 5;

int fail(int code, const std::string& msg) {
    std::cerr << "error: " << msg << '\n';
    return code;
}

void usage() {
    std::cerr <<
        "usage:\n"
        "  motion generate --out DIR\n"
        "  motion analyze --manifest FILE --enter X --exit X --min-frames N\n"
        "                 --gap N --budget BITS --out DIR [--skip]\n";
}

bool parse_double(const std::string& s, double& out) {
    char* end = nullptr;
    out = std::strtod(s.c_str(), &end);
    return end != s.c_str() && *end == '\0' && out == out; // reject NaN
}

bool parse_u64(const std::string& s, uint64_t& out) {
    if (s.empty() || s[0] == '-' || s[0] == '+')
        return false;
    uint64_t v = 0;
    for (char c : s) {
        if (c < '0' || c > '9')
            return false;
        if (v > (UINT64_MAX - 9) / 10)
            return false;
        v = v * 10 + static_cast<uint64_t>(c - '0');
    }
    out = v;
    return true;
}

std::size_t intensity_bucket(double s) {
    int b = static_cast<int>(s * 10.0); // [0,0.1) ... [0.9,1.0]
    if (b < 0)
        b = 0;
    if (b > 9)
        b = 9; // the last bucket includes 1.0
    return static_cast<std::size_t>(b);
}

struct AnalyzeOptions {
    std::string manifest, out;
    double enter = -1.0, exit = -1.0;
    uint64_t min_frames = 0, gap = 0, budget = 0;
    bool have_gap = false, have_budget = false, have_min = false;
    bool skip = false;
};

int parse_analyze_args(int argc, char** argv, AnalyzeOptions& o) {
    for (int i = 2; i < argc; ++i) {
        const std::string a = argv[i];
        auto need_value = [&](const char* name) -> const char* {
            if (i + 1 >= argc)
                throw std::string(name) + " requires a value";
            return argv[++i];
        };
        try {
            if (a == "--manifest")
                o.manifest = need_value("--manifest");
            else if (a == "--out")
                o.out = need_value("--out");
            else if (a == "--enter") {
                if (!parse_double(need_value("--enter"), o.enter))
                    return fail(kExitParam, "bad --enter value");
            } else if (a == "--exit") {
                if (!parse_double(need_value("--exit"), o.exit))
                    return fail(kExitParam, "bad --exit value");
            } else if (a == "--min-frames") {
                if (!parse_u64(need_value("--min-frames"), o.min_frames))
                    return fail(kExitParam, "bad --min-frames value");
                o.have_min = true;
            } else if (a == "--gap") {
                if (!parse_u64(need_value("--gap"), o.gap))
                    return fail(kExitParam, "bad --gap value");
                o.have_gap = true;
            } else if (a == "--budget") {
                if (!parse_u64(need_value("--budget"), o.budget))
                    return fail(kExitParam, "bad --budget value");
                o.have_budget = true;
            } else if (a == "--skip") {
                o.skip = true;
            } else {
                return fail(kExitParam, "unknown argument: " + a);
            }
        } catch (const std::string& msg) {
            return fail(kExitParam, msg);
        }
    }
    if (o.manifest.empty() || o.out.empty() || o.enter < 0.0 || o.exit < 0.0 ||
        !o.have_min || !o.have_gap || !o.have_budget) {
        usage();
        return fail(kExitParam, "missing required analyze arguments");
    }
    if (o.enter > 1.0 || o.exit > 1.0)
        return fail(kExitParam, "thresholds must be within [0,1]");
    if (o.enter < o.exit)
        return fail(kExitParam, "--enter must be >= --exit");
    if (o.min_frames < 1)
        return fail(kExitParam, "--min-frames must be >= 1");
    return -1; // ok
}

int cmd_analyze(const AnalyzeOptions& o) {
    Manifest m;
    try {
        m = load_manifest(o.manifest);
    } catch (const std::exception& e) {
        return fail(kExitInput, e.what());
    }

    Segmenter seg(Segmenter::Params{o.enter, o.exit,
                                    static_cast<std::size_t>(o.min_frames),
                                    static_cast<std::size_t>(o.gap)});
    FrameStream stream;
    std::array<uint64_t, 10> hist{};
    std::vector<FrameRow> rows;
    std::vector<std::pair<std::size_t, double>> measured; // index, intensity
    uint64_t valid_pairs = 0, skips = 0;

    for (const ManifestEntry& e : m.entries) {
        FrameRow row;
        row.index = e.index;
        row.timestamp_us = e.timestamp_us;
        FrameBuffer cur;
        std::string problem;
        try {
            cur = read_ppm(manifest_entry_path(m, e));
        } catch (const std::exception& ex) {
            problem = ex.what();
        }
        double s = 0.0;
        PushResult pr = PushResult::Reference;
        if (problem.empty())
            pr = stream.push(std::move(cur), s, problem);
        if (!problem.empty() && pr != PushResult::Intensity) {
            if (!o.skip)
                return fail(kExitInput, "frame " + std::to_string(e.index) +
                                            " (" + e.relpath + "): " + problem);
            row.ok = false;
            row.note = problem;
            ++skips;
            stream.cut();
            seg.cut();
            rows.push_back(row);
            continue;
        }
        if (pr == PushResult::Intensity) {
            row.has_intensity = true;
            row.intensity = s;
            ++hist[intensity_bucket(s)];
            seg.push(e.index, s);
            measured.emplace_back(e.index, s);
            ++valid_pairs;
        }
        rows.push_back(row);
    }
    seg.finish();

    // Budget allocation over the frames that have an intensity, using the
    // values produced by the shared diff kernel above.
    std::vector<double> intensities;
    intensities.reserve(measured.size());
    for (const auto& kv : measured)
        intensities.push_back(kv.second);
    std::vector<uint64_t> bits;
    try {
        bits = allocate_bits(intensities, o.budget);
    } catch (const std::exception& e) {
        return fail(kExitInput, e.what());
    }
    for (std::size_t i = 0; i < measured.size(); ++i) {
        FrameRow& r = rows[measured[i].first];
        r.has_bits = true;
        r.bits = bits[i];
    }

    // Self-validation before anything touches disk.
    const uint64_t hist_total =
        std::accumulate(hist.begin(), hist.end(), uint64_t{0});
    if (hist_total != valid_pairs)
        return fail(kExitValidate, "histogram total != valid pairs");
    if (!bits.empty()) {
        const uint64_t bit_total =
            std::accumulate(bits.begin(), bits.end(), uint64_t{0});
        if (bit_total != o.budget)
            return fail(kExitValidate, "bit budget not conserved");
    }
    const std::vector<Segment>& segs = seg.segments();
    for (std::size_t i = 0; i < segs.size(); ++i) {
        if (segs[i].begin >= segs[i].end ||
            (i && segs[i].begin <= segs[i - 1].end))
            return fail(kExitValidate, "segment invariant violated");
    }

    // Write results via temporary files; no official output is left
    // behind unless every stage succeeded.
    namespace fs = std::filesystem;
    const std::string frames_csv = (fs::path(o.out) / "frames.csv").string();
    const std::string segs_csv = (fs::path(o.out) / "segments.csv").string();
    const std::string hist_ppm = (fs::path(o.out) / "histogram.ppm").string();
    HistogramSummary summary;
    try {
        std::error_code ec;
        fs::create_directories(o.out, ec);
        if (ec)
            throw std::runtime_error("cannot create directory: " + o.out);
        const std::string t1 = frames_csv + ".tmp", t2 = segs_csv + ".tmp",
                          t3 = hist_ppm + ".tmp";
        try {
            write_frames_csv(t1, rows);
            write_segments_csv(t2, segs);
            summary = write_histogram_ppm(t3, hist);
            fs::rename(t1, frames_csv);
            fs::rename(t2, segs_csv);
            fs::rename(t3, hist_ppm);
        } catch (...) {
            fs::remove(t1, ec);
            fs::remove(t2, ec);
            fs::remove(t3, ec);
            throw;
        }
    } catch (const std::exception& e) {
        return fail(kExitWrite, e.what());
    }

    // Terminal report.
    uint64_t n_zero = 0, n_one = 0;
    for (const auto& kv : measured) {
        if (kv.second == 0.0)
            ++n_zero;
        if (kv.second == 1.0)
            ++n_one;
    }
    for (const FrameRow& r : rows) {
        std::cout << "frame index=" << r.index << " ts=" << r.timestamp_us;
        if (!r.ok)
            std::cout << " skipped";
        if (r.has_intensity) {
            char buf[32];
            std::snprintf(buf, sizeof buf, "%.6f", r.intensity);
            std::cout << " intensity=" << buf;
        } else {
            std::cout << " intensity=-";
        }
        if (r.has_bits)
            std::cout << " bits=" << r.bits;
        else
            std::cout << " bits=-";
        std::cout << '\n';
        if (!r.ok)
            std::cout << "skip index=" << r.index << " reason=" << r.note
                      << '\n';
    }
    std::cout << "frame_rows=" << rows.size() << '\n';
    std::cout << "valid_pairs=" << valid_pairs << '\n';
    std::cout << "intensity_0_count=" << n_zero << '\n';
    std::cout << "intensity_1_count=" << n_one << '\n';
    std::cout << "histogram=[";
    for (std::size_t i = 0; i < hist.size(); ++i)
        std::cout << (i ? "," : "") << hist[i];
    std::cout << "]\n";
    std::cout << "segments=";
    for (std::size_t i = 0; i < segs.size(); ++i)
        std::cout << (i ? "," : "") << '[' << segs[i].begin << ','
                  << segs[i].end << ')';
    std::cout << '\n';
    std::cout << "skips=" << skips << '\n';
    std::cout << "summary_pixels_sum=" << summary.pixels_sum << '\n';
    char hash_buf[32];
    std::snprintf(hash_buf, sizeof hash_buf, "%016llx",
                  static_cast<unsigned long long>(summary.fnv));
    std::cout << "summary_fnv1a64=" << hash_buf << '\n';
    std::cout << "outputs=" << o.out << '\n';
    return kExitOk;
}

int cmd_generate(const std::string& out) {
    if (out.empty()) {
        usage();
        return fail(kExitParam, "generate requires --out DIR");
    }
    try {
        generate_fixture(out);
    } catch (const std::exception& e) {
        return fail(kExitWrite, e.what());
    }
    std::cout << "fixture=" << out << '\n';
    std::cout << "frames=12\n";
    return kExitOk;
}

} // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        usage();
        return kExitParam;
    }
    const std::string cmd = argv[1];
    if (cmd == "generate") {
        std::string out;
        for (int i = 2; i < argc; ++i) {
            const std::string a = argv[i];
            if (a == "--out" && i + 1 < argc)
                out = argv[++i];
            else
                return fail(kExitParam, "unknown argument: " + a);
        }
        return cmd_generate(out);
    }
    if (cmd == "analyze") {
        AnalyzeOptions o;
        const int rc = parse_analyze_args(argc, argv, o);
        if (rc >= 0)
            return rc;
        return cmd_analyze(o);
    }
    usage();
    return fail(kExitParam, "unknown subcommand: " + cmd);
}

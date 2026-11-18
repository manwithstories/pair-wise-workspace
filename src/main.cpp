#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>

#include "fixture.hpp"

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
        (void)o;
        return fail(kExitInput, "analyze pipeline not wired yet");
    }
    usage();
    return fail(kExitParam, "unknown subcommand: " + cmd);
}

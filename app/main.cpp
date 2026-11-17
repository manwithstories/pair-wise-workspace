// SPDX-License-Identifier: MIT
// app/main.cpp — CLI entry: generate / convert / audit / self-test.
//
// Exit codes (contract): 0 success, 2 parse/data-format failure,
// 3 audit found data problems, 4 bad arguments/usage.
#include <CLI/CLI.hpp>
#include <fmt/format.h>

#include <fstream>
#include <iostream>
#include <string>
#include <vector>

#include "timecode/audit.hpp"
#include "timecode/converter.hpp"
#include "timecode/fixture_generator.hpp"
#include "timecode/jsonl.hpp"
#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

using namespace tc;

namespace {

constexpr int kOk = 0;
constexpr int kParseError = 2;
constexpr int kAuditProblem = 3;
constexpr int kUsageError = 4;

Rate require_rate(const std::string& text) {
    auto r = parse_rate(text);
    if (!r) throw CLI::ValidationError(fmt::format("unknown format '{}'", text));
    return *r;
}

WrapPolicy parse_policy(const std::string& text) {
    if (text == "strict") return WrapPolicy::Strict;
    if (text == "wrap") return WrapPolicy::Wrap;
    if (text == "repair") return WrapPolicy::Repair;
    throw CLI::ValidationError("wrap policy must be strict|wrap|repair");
}

std::vector<AuditInput> read_lines(const std::string& path) {
    std::ifstream in(path);
    if (!in) throw CLI::ValidationError(fmt::format("cannot read '{}'", path));
    std::vector<AuditInput> out;
    std::string line;
    while (std::getline(in, line)) {
        // JSONL fixture inputs may carry an explicit step field.
        std::int64_t step = 0;
        std::string text = line;
        if (!line.empty() && line.front() == '{') {
            try {
                Json j = Json::parse(line);
                text = j.value("timecode", j.value("text", std::string{}));
                step = j.value("step", 0);
            } catch (...) {
                // Treat unparseable JSON as a raw line; the parser reports it.
                text = line;
            }
        }
        out.push_back(AuditInput{text, step});
    }
    return out;
}

int cmd_generate(const std::string& profile_text, const std::string& format,
                 const std::string& out_path) {
    auto profile = parse_profile(profile_text);
    if (!profile) {
        std::cerr << fmt::format("unknown profile '{}'\n", profile_text);
        return kUsageError;
    }
    Rate rate = require_rate(format);
    Fixture fx = build_fixture(*profile, rate);

    std::ofstream out(out_path);
    if (!out) {
        std::cerr << fmt::format("cannot write '{}'\n", out_path);
        return kUsageError;
    }
    for (const FixtureLine& l : fx.lines) {
        Json j;
        j["timecode"] = l.text;
        j["step"] = l.step;
        if (l.inject_error) j["inject_error"] = true;
        if (!l.note.empty()) j["note"] = l.note;
        out << j.dump() << '\n';
    }
    std::cout << Json{{"generated", fx.lines.size()},
                      {"profile", profile_text},
                      {"format", rate_info(rate).canonical},
                      {"out", out_path}}
                     .dump()
              << '\n';
    return kOk;
}

int cmd_convert(const std::string& value, const std::string& from,
                const std::string& to, bool show_frame) {
    Rate src_rate = require_rate(from);
    Rate dst_rate = require_rate(to);
    ParseResult pr = parse_timecode(value, src_rate);
    if (!pr.ok) {
        std::cout << Json{{"ok", false},
                          {"raw", value},
                          {"error", diag_name(pr.diag)},
                          {"message", pr.message}}
                             .dump()
                  << '\n';
        return kParseError;
    }
    CrossRateResult cr = convert_rate(pr.tc, src_rate, dst_rate);
    Json j;
    j["ok"] = true;
    j["raw"] = value;
    j["source_format"] = rate_info(src_rate).canonical;
    j["target_format"] = rate_info(dst_rate).canonical;
    j["timecode"] = format_timecode(cr.target_tc, dst_rate);
    j["source_frame"] = cr.source_frame;
    j["target_frame"] = cr.target_frame;
    j["exact"] = cr.exact;
    j["warnings"] = cr.warnings;
    if (show_frame) j["show_frame"] = true;
    std::cout << j.dump() << '\n';
    return kOk;
}

int cmd_audit(const std::string& input, const std::string& format,
              const std::string& policy_text, bool) {
    Rate rate = require_rate(format);
    WrapPolicy policy = parse_policy(policy_text);
    std::vector<AuditInput> lines = read_lines(input);
    AuditOutput out = audit_sequence(lines, rate, policy);

    for (const Record& rec : out.records)
        std::cout << record_json(rec, rate).dump() << '\n';
    for (const AuditEvent& ev : out.events) {
        std::cout << Json{{"event", diag_name(ev.diag)},
                          {"line", ev.line},
                          {"message", ev.message},
                          {"raw", ev.raw}}
                             .dump()
                  << '\n';
    }
    std::cout << summary_json(out.summary).dump() << '\n';

    if (out.summary.invalid > 0) return kParseError;
    if (!out.summary.clean()) return kAuditProblem;
    return kOk;
}

int cmd_self_test() {
    // In-process deterministic checks mirroring the acceptance invariants, so
    // the binary can be smoke-tested without the Catch2 harness.
    Json report;
    bool all_ok = true;

    auto check = [&](const std::string& name, bool cond) {
        report[name] = cond;
        all_ok = all_ok && cond;
    };

    auto pr = parse_timecode("00:01:00;02", Rate::F2997_DF);
    check("parse_df", pr.ok && to_frame_number(pr.tc, Rate::F2997_DF) == 1800);
    check("illegal_drop",
          parse_timecode("00:01:00;00", Rate::F2997_DF).diag ==
              Diag::IllegalDropFrame);

    bool roundtrip = true;
    const RateInfo info = rate_info(Rate::F2997_DF);
    for (std::int64_t f = 0; f < info.frames_per_day; f += 997) {
        auto back = from_frame_number(f, Rate::F2997_DF);
        roundtrip = roundtrip && to_frame_number(back.tc, Rate::F2997_DF) == f;
    }
    check("df_roundtrip_zero_error", roundtrip);
    check("twelve_hour_compensation_43164",
          twelve_hour_compensation(Rate::F2997_DF) == 43164);

    auto fx = build_fixture(FixtureProfile::Overnight, Rate::F2997_DF);
    std::vector<AuditInput> lines;
    for (const auto& l : fx.lines) lines.push_back(AuditInput{l.text, l.step});
    auto overnight = audit_sequence(lines, Rate::F2997_DF, WrapPolicy::Repair);
    check("overnight_single_wrap", overnight.summary.wraps == 1);
    check("overnight_clean", overnight.summary.clean());

    report["ok"] = all_ok;
    std::cout << report.dump(2) << '\n';
    return all_ok ? kOk : kAuditProblem;
}

}  // namespace

int main(int argc, char** argv) {
    CLI::App app{"Local timecode parse / convert / audit tool"};
    app.require_subcommand(1);

    // generate
    auto* gen = app.add_subcommand("generate", "Emit a deterministic fixture");
    std::string g_profile = "overnight";
    std::string g_format = "2997df";
    std::string g_out;
    gen->add_option("--profile", g_profile, "boundary|overnight|longrun|anomalies");
    gen->add_option("--format", g_format, "rate name, e.g. 2997df");
    gen->add_option("--out", g_out, "output JSONL path")->required();

    // convert
    auto* conv = app.add_subcommand("convert", "Convert one timecode");
    std::string c_value, c_from, c_to;
    bool c_show_frame = false;
    conv->add_option("--value", c_value, "timecode text")->required();
    conv->add_option("--from", c_from, "source format")->required();
    conv->add_option("--to", c_to, "target format")->required();
    conv->add_flag("--show-frame", c_show_frame);

    // audit
    auto* aud = app.add_subcommand("audit", "Audit a JSONL/timecode stream");
    std::string a_input, a_format = "2997df", a_wrap = "repair";
    bool a_json = true;
    aud->add_option("--input", a_input, "input file")->required();
    aud->add_option("--format", a_format);
    aud->add_option("--wrap", a_wrap, "strict|wrap|repair");
    aud->add_flag("--json", a_json);

    // self-test
    app.add_subcommand("self-test", "Run built-in acceptance invariants");

    try {
        app.parse(argc, argv);
    } catch (const CLI::ParseError& e) {
        return app.exit(e);  // CLI11 maps usage failures to non-zero
    }

    try {
        if (*gen) return cmd_generate(g_profile, g_format, g_out);
        if (*conv) return cmd_convert(c_value, c_from, c_to, c_show_frame);
        if (*aud) return cmd_audit(a_input, a_format, a_wrap, a_json);
        if (app.get_subcommand("self-test")->parsed()) return cmd_self_test();
    } catch (const CLI::ValidationError& e) {
        std::cerr << fmt::format("argument error: {}\n", e.what());
        return kUsageError;
    }
    return kOk;
}

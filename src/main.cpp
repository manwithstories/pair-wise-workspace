#include <cstdint>
#include <algorithm>
#include <filesystem>
#include <iostream>
#include <set>
#include <sstream>
#include <string>
#include <vector>

#include "gen_fixtures.hpp"
#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/command_utils.hpp"
#include "subtitle_audit/layout_probe.hpp"
#include "subtitle_audit/report.hpp"
#include "subtitle_audit/style_chain.hpp"
#include "subtitle_audit/tag_lexer.hpp"
#include "subtitle_audit/version.hpp"

namespace {

bool check_parse(const audit::AssDocument &doc) {
    print_diagnostics(doc);
    if (!doc.valid()) {
        std::cerr << "parse error: " << doc.parse_error << '\n';
        return false;
    }
    return true;
}

bool check_chains(const audit::ChainAnalysis &chains) {
    bool ok = true;
    for (const auto &error : chains.errors) {
        std::cerr << "chain error: line " << error.line << ": " << error.message << '\n';
        ok = false;
    }
    return ok;
}

void usage(std::ostream &out) {
    out << "subtitle-audit " << audit::kToolVersion << "\n"
        << "usage: subtitle-audit <command> [options]\n"
        << "  gen-fixtures --out DIR [--count N] [--seed N]\n"
        << "  style-chain --script FILE [--json FILE]\n"
        << "  tag-stats --script FILE [--top N] [--json FILE]\n"
        << "  render-probe --script FILE [--frame MS] [--out FILE] [--dump-alpha DIR]\n"
        << "  batch --dir DIR [--out FILE] [--frame MS]\n";
}

int command_gen_fixtures(int argc, char **argv) {
    const auto options = audit::parse_cli_options(argc, argv, 2);
    std::string error = options.error;
    if (error.empty() &&
        !audit::check_cli_options(options, {"out", "count", "seed"}, {"out"}, error)) {}
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    audit::FixtureOptions fixtures;
    fixtures.output_dir = options.values.at("out");
    fixtures.count = audit::parse_int_option(options, "count", 12, 1, 999, error);
    if (error.empty() && options.values.count("seed")) {
        try { fixtures.seed = std::stoull(options.values.at("seed")); }
        catch (...) { error = "invalid --seed"; }
    }
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    if (!audit::generate_fixtures(fixtures, error)) {
        std::cerr << "error: " << error << '\n';
        return 2;
    }
    std::cout << "generated " << fixtures.count << " scripts in " << fixtures.output_dir << '\n';
    return 0;
}

int command_style_chain(int argc, char **argv) {
    const auto options = audit::parse_cli_options(argc, argv, 2);
    std::string error = options.error;
    if (error.empty() &&
        !audit::check_cli_options(options, {"script", "json"}, {"script"}, error)) {}
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const auto doc = audit::parse_ass_file(options.values.at("script"));
    if (!check_parse(doc)) return 3;
    const auto chains = audit::analyze_style_chains(doc);
    if (!check_chains(chains)) return 3;
    for (const auto &event : doc.events) {
        if (!doc.find_style(event.style)) {
            std::cerr << "chain error: line " << event.line << ": event references missing style '"
                      << event.style << "'\n";
            return 3;
        }
        const auto lexed = audit::lex_event_text(event.text);
        for (const auto &tag : lexed.tags) {
            if (tag.name != "r" || audit::trim(tag.argument).empty()) continue;
            const std::string style = audit::trim(tag.argument);
            if (!doc.find_style(style)) {
                std::cerr << "chain error: line " << event.line
                          << ": override references missing style '" << style << "'\n";
                return 3;
            }
        }
    }
    const std::string json = audit::chain_report_json(doc, chains);
    if (options.values.count("json")) {
        if (options.values.at("json") == "-") std::cout << json << '\n';
        else if (!audit::write_text_file(options.values.at("json"), json)) {
            std::cerr << "error: cannot write JSON output\n";
            return 2;
        }
    }
    std::cout << "events " << doc.events.size() << '\n'
              << "styles " << doc.styles.size() << '\n'
              << "max_chain_depth " << chains.max_depth << '\n'
              << "cycles " << chains.cycles << '\n';
    const auto chain = audit::deepest_chain(chains);
    if (!chain.empty()) {
        std::cout << "chain ";
        for (std::size_t i = 0; i < chain.size(); ++i) std::cout << (i ? " -> " : "") << chain[i];
        std::cout << '\n';
    }
    return 0;
}

int command_tag_stats(int argc, char **argv) {
    const auto options = audit::parse_cli_options(argc, argv, 2);
    std::string error = options.error;
    if (error.empty() &&
        !audit::check_cli_options(options, {"script", "top", "json"}, {"script"}, error)) {}
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const int top = audit::parse_int_option(options, "top", 5, 0, 1000, error);
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const auto doc = audit::parse_ass_file(options.values.at("script"));
    if (!check_parse(doc)) return 3;
    const auto tags = audit::aggregate_tags(doc);
    const std::string json = audit::tag_report_json(doc, tags);
    if (options.values.count("json")) {
        if (options.values.at("json") == "-") std::cout << json << '\n';
        else if (!audit::write_text_file(options.values.at("json"), json)) {
            std::cerr << "error: cannot write JSON output\n";
            return 2;
        }
    }
    std::cout << "tags " << tags.total << '\n' << "unknown " << tags.unknown << '\n'
              << "t_nested " << tags.t_nested << '\n' << "r_switch " << tags.r_switch << '\n'
              << "top:\n";
    for (const auto &[name, count] : tags.top(top))
        std::cout << "  \\" << name << ' ' << count << '\n';
    return 0;
}

int command_render_probe(int argc, char **argv) {
    const auto options = audit::parse_cli_options(argc, argv, 2);
    std::string error = options.error;
    if (error.empty() &&
        !audit::check_cli_options(options, {"script", "frame", "out", "dump-alpha"},
                                  {"script"}, error)) {}
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const int64_t frame = audit::parse_int_option(options, "frame", 1250, 0, 86400000, error);
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const auto doc = audit::parse_ass_file(options.values.at("script"));
    if (!check_parse(doc)) return 3;
    const auto chains = audit::analyze_style_chains(doc);
    if (!check_chains(chains)) return 3;
    const std::string dump = options.values.count("dump-alpha")
        ? options.values.at("dump-alpha") : "";
    const auto probe = audit::render_probe(doc, frame, dump);
    if (!probe.ok) {
        std::cerr << "render error: " << probe.error << '\n';
        return 4;
    }
    const std::string json = audit::probe_report_json(doc, probe);
    if (options.values.count("out")) {
        if (options.values.at("out") == "-") std::cout << json << '\n';
        else if (!audit::write_text_file(options.values.at("out"), json)) {
            std::cerr << "error: cannot write JSON output\n";
            return 2;
        }
    }
    for (const auto &event : probe.events) {
        if (!event.visible) continue;
        std::cout << "event " << event.index << " line " << event.line << " visible bbox="
                  << event.bbox.x << ',' << event.bbox.y << ',' << event.bbox.w << ','
                  << event.bbox.h << " w=" << event.bbox.w << " h=" << event.bbox.h
                  << " fnv1a=" << event.alpha_fnv1a << '\n';
    }
    std::cout << "visible " << probe.visible << '\n' << "max_width " << probe.max_width << '\n'
              << "max_height " << probe.max_height << '\n';
    if (!probe.missing_fonts.empty()) {
        std::cout << "missing_fonts";
        for (const auto &font : probe.missing_fonts) std::cout << ' ' << font;
        std::cout << '\n';
        return 5;
    }
    return 0;
}

}  // namespace

int main(int argc, char **argv) {
    if (argc < 2) { usage(std::cerr); return 2; }
    const std::string command = argv[1];
    if (command == "--help" || command == "-h" || command == "help") {
        usage(std::cout);
        return 0;
    }
    if (command == "--version") { std::cout << audit::kToolVersion << '\n'; return 0; }
    if (command == "gen-fixtures") return command_gen_fixtures(argc, argv);
    if (command == "style-chain") return command_style_chain(argc, argv);
    if (command == "tag-stats") return command_tag_stats(argc, argv);
    if (command == "render-probe") return command_render_probe(argc, argv);
    if (command == "batch") return audit::run_batch_command(argc, argv);
    std::cerr << "error: unknown command: " << command << '\n';
    usage(std::cerr);
    return 2;
}

#include <algorithm>
#include <atomic>
#include <charconv>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include "cue_parse.hpp"
#include "font_face.hpp"
#include "layout.hpp"
#include "report.hpp"
#include "selftest.hpp"
#include "ttf_synth.hpp"

namespace fs = std::filesystem;
using namespace smx;

namespace {
constexpr int kExitOk = 0;
constexpr int kExitOverflow = 2;
constexpr int kExitInput = 3;
constexpr int kExitSelfTest = 4;
constexpr int kExitUsage = 64;

struct Args {
  std::map<std::string, std::vector<std::string>> values;
  bool has(const std::string& key) const { return values.count(key) != 0; }
  std::string one(const std::string& key, const std::string& fallback = "") const {
    auto it = values.find(key);
    return it == values.end() || it->second.empty() ? fallback : it->second.back();
  }
  std::vector<std::string> many(const std::string& key) const {
    auto it = values.find(key);
    return it == values.end() ? std::vector<std::string>{} : it->second;
  }
};

bool parse_args(int argc, char** argv, Args& args, std::string& error) {
  for (int i = 2; i < argc; ++i) {
    std::string token = argv[i];
    if (token.rfind("--", 0) != 0 || token.size() == 2) {
      error = "unexpected argument: " + token;
      return false;
    }
    token = token.substr(2);
    std::string value;
    const auto eq = token.find('=');
    if (eq != std::string::npos) {
      value = token.substr(eq + 1);
      token = token.substr(0, eq);
    } else {
      if (i + 1 >= argc) {
        error = "missing value for --" + token;
        return false;
      }
      value = argv[++i];
    }
    if (token.empty()) {
      error = "empty option name";
      return false;
    }
    args.values[token].push_back(value);
  }
  return true;
}

bool validate_keys(const Args& args, const std::set<std::string>& allowed,
                   std::string& error, bool allow_repeat_fallback = true) {
  for (const auto& entry : args.values) {
    if (allowed.count(entry.first) == 0) {
      error = "unknown option: --" + entry.first;
      return false;
    }
    const bool repeatable = allow_repeat_fallback && entry.first == "fallback";
    if (!repeatable && entry.second.size() > 1) {
      error = "option given more than once: --" + entry.first;
      return false;
    }
  }
  return true;
}

bool parse_u64(const std::string& text, std::uint64_t& value) {
  if (text.empty()) return false;
  const auto* first = text.data();
  const auto* last = first + text.size();
  auto result = std::from_chars(first, last, value);
  return result.ec == std::errc() && result.ptr == last;
}

bool parse_int(const std::string& text, int& value) {
  if (text.empty()) return false;
  int parsed = 0;
  const auto* first = text.data();
  const auto* last = first + text.size();
  auto result = std::from_chars(first, last, parsed);
  if (result.ec != std::errc() || result.ptr != last) return false;
  value = parsed;
  return true;
}

bool ensure_parent(const std::string& path, std::string& error) {
  const fs::path p(path);
  if (!p.has_parent_path()) return true;
  std::error_code ec;
  fs::create_directories(p.parent_path(), ec);
  if (ec) {
    error = "cannot create directory " + p.parent_path().string() + ": " + ec.message();
    return false;
  }
  return true;
}

std::vector<std::string> fallback_paths(const Args& args) {
  std::vector<std::string> out;
  for (const auto& item : args.many("fallback")) {
    std::size_t start = 0;
    while (start <= item.size()) {
      const auto comma = item.find(',', start);
      const auto end = comma == std::string::npos ? item.size() : comma;
      const auto part = item.substr(start, end - start);
      if (!part.empty()) out.push_back(part);
      if (comma == std::string::npos) break;
      start = comma + 1;
    }
  }
  return out;
}

int fail(int code, const std::string& message) {
  std::cerr << "smx: " << message << '\n';
  return code;
}

void print_usage() {
  std::cerr
      << "usage:\n"
      << "  smx genfont --out FILE --seed N\n"
      << "  smx measure --font FILE --size PX --max-width PX --input FILE --out FILE\n"
      << "              [--max-lines N] [--layout dp|greedy] [--truncate keep|ellipsis]\n"
      << "              [--fallback FILE[,FILE...]] [--tsv FILE]\n"
      << "  smx batch --font FILE --size PX --max-width PX --dir DIR --out-dir DIR\n"
      << "            --jobs N [--max-lines N] [--layout dp|greedy] [--truncate keep|ellipsis]\n"
      << "            [--fallback FILE[,FILE...]]\n"
      << "  smx selftest\n";
}

struct CommonOptions {
  std::string font;
  std::vector<std::string> fallbacks;
  int size = 0;
  int max_width = 0;
  int max_lines = 0;
  LayoutOptions layout;
};

bool read_common(const Args& args, CommonOptions& options, std::string& error) {
  options.font = args.one("font");
  options.fallbacks = fallback_paths(args);
  if (options.font.empty()) { error = "--font is required"; return false; }
  if (!parse_int(args.one("size"), options.size) || options.size <= 0) {
    error = "--size must be a positive integer"; return false;
  }
  if (!parse_int(args.one("max-width"), options.max_width) || options.max_width <= 0) {
    error = "--max-width must be a positive integer"; return false;
  }
  if (args.has("max-lines") &&
      (!parse_int(args.one("max-lines"), options.max_lines) || options.max_lines < 0)) {
    error = "--max-lines must be a non-negative integer"; return false;
  }
  const std::string layout = args.one("layout", "dp");
  if (layout == "dp") options.layout.strategy = LayoutStrategy::Dp;
  else if (layout == "greedy") options.layout.strategy = LayoutStrategy::Greedy;
  else { error = "--layout must be dp or greedy"; return false; }
  const std::string truncate = args.one("truncate", "keep");
  if (truncate == "keep") options.layout.truncate = TruncatePolicy::Keep;
  else if (truncate == "ellipsis") options.layout.truncate = TruncatePolicy::Ellipsis;
  else { error = "--truncate must be keep or ellipsis"; return false; }
  options.layout.max_width26 = options.max_width * 64;
  options.layout.max_lines = options.max_lines;
  return true;
}

bool validate_measure_args(const Args& args, std::string& error) {
  if (args.one("input").empty()) { error = "--input is required"; return false; }
  if (args.one("out").empty()) { error = "--out is required"; return false; }
  return true;
}

int command_genfont(const Args& args) {
  std::string error;
  if (!validate_keys(args, {"out", "seed"}, error)) return fail(kExitUsage, error);
  const std::string out = args.one("out");
  if (out.empty()) return fail(kExitUsage, "genfont requires --out");
  std::uint64_t seed = 0;
  if (!parse_u64(args.one("seed"), seed))
    return fail(kExitUsage, "genfont requires --seed as an unsigned integer");
  if (!ensure_parent(out, error)) return fail(kExitInput, error);
  if (!write_synth_font(out, seed, error)) return fail(kExitInput, error);
  std::cout << "genfont: wrote " << out << " seed=" << seed << '\n';
  return kExitOk;
}

int command_measure(const Args& args) {
  std::string error;
  if (!validate_keys(args,
                     {"font", "size", "max-width", "max-lines", "input", "out",
                      "tsv", "fallback", "layout", "truncate"},
                     error))
    return fail(kExitUsage, error);
  if (!validate_measure_args(args, error)) return fail(kExitUsage, error);
  CommonOptions options;
  if (!read_common(args, options, error)) return fail(kExitUsage, error);
  std::string io_error;
  const CueDocument document = parse_cue_file(args.one("input"), io_error);
  if (!io_error.empty()) return fail(kExitInput, io_error);
  if (!document.errors.empty()) {
    std::ostringstream ss;
    ss << args.one("input") << ':' << document.errors.front().line << ": "
       << document.errors.front().message;
    return fail(kExitInput, ss.str());
  }
  FontFace font;
  if (!font.open(options.font, options.fallbacks, options.size, error))
    return fail(kExitInput, error);
  Report report = build_report(document, font, options.layout);
  const std::string out = args.one("out");
  if (!ensure_parent(out, error)) return fail(kExitInput, error);
  if (!write_text_file(out, render_report_json(report), error)) return fail(kExitInput, error);
  if (args.has("tsv")) {
    const std::string tsv = args.one("tsv");
    if (!ensure_parent(tsv, error)) return fail(kExitInput, error);
    if (!write_text_file(tsv, render_report_tsv(report), error)) return fail(kExitInput, error);
  }
  std::cout << "measure: cues=" << report.cues_total << " ok=" << report.cues_ok
            << " overflow=" << report.overflow_total << " lines=" << report.lines_total
            << " max_width_px=" << format_fixed3(report.max_line_width26)
            << " hash=" << report.report_hash << '\n';
  return report.overflow_total == 0 ? kExitOk : kExitOverflow;
}

struct BatchFile {
  std::string name;
  std::string path;
  std::string output;
  std::string error;
  bool failed = false;
  bool has_report = false;
  Report report;
};

std::string batch_json_without_hash(const std::vector<BatchFile>& files, std::size_t failed,
                                    const std::string& missing_font_error) {
  std::size_t cues_total = 0, cues_ok = 0, overflow_total = 0, lines_total = 0;
  std::size_t width = 0, lines = 0, unbreakable = 0, missing = 0, invalid_utf8 = 0;
  int max_width = 0;
  for (const auto& f : files) {
    if (!f.has_report) continue;
    cues_total += f.report.cues_total;
    cues_ok += f.report.cues_ok;
    overflow_total += f.report.overflow_total;
    lines_total += f.report.lines_total;
    invalid_utf8 += f.report.invalid_utf8_total;
    width += f.report.overflow_width;
    lines += f.report.overflow_lines;
    unbreakable += f.report.overflow_unbreakable;
    missing += f.report.overflow_missing_glyph;
    max_width = std::max(max_width, f.report.max_line_width26);
  }
  std::string out = "{\n";
  out += "  \"schema\":\"smx-batch-v1\",\n";
  out += "  \"files_total\":" + std::to_string(files.size()) + ",\n";
  out += "  \"files_failed\":" + std::to_string(failed) + ",\n";
  out += "  \"cues_total\":" + std::to_string(cues_total) + ",\n";
  out += "  \"cues_ok\":" + std::to_string(cues_ok) + ",\n";
  out += "  \"overflow_total\":" + std::to_string(overflow_total) + ",\n";
  out += "  \"overflow\":{\"width\":" + std::to_string(width) +
         ",\"lines\":" + std::to_string(lines) +
         ",\"unbreakable\":" + std::to_string(unbreakable) +
         ",\"missing_glyph\":" + std::to_string(missing) + "},\n";
  out += "  \"lines_total\":" + std::to_string(lines_total) + ",\n";
  out += "  \"max_line_width_px\":" + format_fixed3(max_width) + ",\n";
  out += "  \"invalid_utf8_total\":" + std::to_string(invalid_utf8) + ",\n";
  if (!missing_font_error.empty())
    out += "  \"font_error\":\"" + json_escape(missing_font_error) + "\",\n";
  out += "  \"files\":[\n";
  for (std::size_t i = 0; i < files.size(); ++i) {
    const auto& f = files[i];
    out += "    {\"name\":\"" + json_escape(f.name) + "\",\"output\":\"" +
           json_escape(f.output) + "\",\"cues_total\":" +
           std::to_string(f.has_report ? f.report.cues_total : 0) +
           ",\"cues_ok\":" + std::to_string(f.has_report ? f.report.cues_ok : 0) +
           ",\"overflow_total\":" +
           std::to_string(f.has_report ? f.report.overflow_total : 0) +
           ",\"lines_total\":" + std::to_string(f.has_report ? f.report.lines_total : 0) +
           ",\"invalid_utf8_total\":" +
           std::to_string(f.has_report ? f.report.invalid_utf8_total : 0) +
           ",\"report_hash\":\"" + (f.has_report ? f.report.report_hash : "") +
           "\",\"error\":\"" + json_escape(f.error) + "\"}";
    if (i + 1 < files.size()) out += ",";
    out += "\n";
  }
  out += "  ]\n}\n";
  return out;
}

int command_batch(const Args& args) {
  std::string error;
  if (!validate_keys(args,
                     {"font", "size", "max-width", "max-lines", "dir", "out-dir",
                      "jobs", "fallback", "layout", "truncate"},
                     error))
    return fail(kExitUsage, error);
  CommonOptions options;
  if (!read_common(args, options, error)) return fail(kExitUsage, error);
  const std::string dir = args.one("dir");
  const std::string out_dir = args.one("out-dir");
  if (dir.empty()) return fail(kExitUsage, "batch requires --dir");
  if (out_dir.empty()) return fail(kExitUsage, "batch requires --out-dir");
  int jobs = 0;
  if (!parse_int(args.one("jobs"), jobs) || jobs <= 0)
    return fail(kExitUsage, "batch requires --jobs as a positive integer");
  std::error_code ec;
  if (!fs::is_directory(dir, ec)) return fail(kExitInput, "input directory not found: " + dir);
  if (!ensure_parent(out_dir + "/batch.json", error)) return fail(kExitInput, error);

  std::vector<fs::path> inputs;
  fs::directory_iterator iterator(dir, ec);
  if (ec) return fail(kExitInput, "cannot read input directory: " + ec.message());
  for (const auto& entry : iterator) {
    std::error_code entry_error;
    if (entry.is_regular_file(entry_error) && !entry_error) inputs.push_back(entry.path());
  }
  std::sort(inputs.begin(), inputs.end(), [](const fs::path& a, const fs::path& b) {
    return a.filename().string() < b.filename().string();
  });

  FontFace probe;
  if (!probe.open(options.font, options.fallbacks, options.size, error))
    return fail(kExitInput, error);

  std::vector<BatchFile> files(inputs.size());
  for (std::size_t i = 0; i < inputs.size(); ++i) {
    files[i].name = inputs[i].filename().string();
    files[i].path = inputs[i].string();
    files[i].output = files[i].name + ".json";
  }
  std::atomic<std::size_t> next{0};
  const std::size_t workers = std::min<std::size_t>(
      static_cast<std::size_t>(jobs), std::max<std::size_t>(1, inputs.size()));
  std::vector<std::thread> threads;
  for (std::size_t t = 0; t < workers; ++t) {
    threads.emplace_back([&]() {
      while (true) {
        const std::size_t i = next.fetch_add(1);
        if (i >= files.size()) break;
        auto& file = files[i];
        std::string io_error;
        const CueDocument document = parse_cue_file(file.path, io_error);
        if (!io_error.empty()) {
          file.error = io_error;
          file.failed = true;
          continue;
        }
        if (!document.errors.empty()) {
          std::ostringstream ss;
          ss << file.name << ':' << document.errors.front().line << ": "
             << document.errors.front().message;
          file.error = ss.str();
          file.failed = true;
        }
        FontFace font;
        std::string open_error;
        if (!font.open(options.font, options.fallbacks, options.size, open_error)) {
          file.error = open_error;
          file.failed = true;
          continue;
        }
        file.report = build_report(document, font, options.layout);
        file.has_report = true;
        std::string write_error;
        const std::string output_path = (fs::path(out_dir) / file.output).string();
        if (!write_text_file(output_path, render_report_json(file.report), write_error)) {
          file.error = write_error;
          file.failed = true;
          continue;
        }
        const std::string tsv_path =
            (fs::path(out_dir) / (file.name + ".tsv")).string();
        if (!write_text_file(tsv_path, render_report_tsv(file.report), write_error)) {
          file.error = write_error;
          file.failed = true;
        }
      }
    });
  }
  for (auto& thread : threads) thread.join();

  std::size_t failed = 0;
  bool any_overflow = false;
  for (const auto& file : files) {
    if (file.failed) ++failed;
    if (file.has_report && file.report.overflow_total > 0) any_overflow = true;
  }
  std::string batch_without = batch_json_without_hash(files, failed, "");
  char hashbuf[17];
  std::snprintf(hashbuf, sizeof(hashbuf), "%016llx",
                static_cast<unsigned long long>(fnv1a64(batch_without)));
  const std::string marker = "  \"files\":[\n";
  const auto at = batch_without.find(marker);
  std::string batch = batch_without.substr(0, at);
  batch += "  \"report_hash\":\"" + std::string(hashbuf) + "\",\n";
  batch += batch_without.substr(at);
  const std::string batch_path = (fs::path(out_dir) / "batch.json").string();
  if (!write_text_file(batch_path, batch, error)) return fail(kExitInput, error);
  std::ostringstream batch_tsv;
  batch_tsv << "file\tcues_total\tcues_ok\toverflow_total\tlines_total"
               "\tmax_line_width_px\tinvalid_utf8_total\treport_hash\terror\n";
  for (const auto& file : files) {
    batch_tsv << file.name << '\t' << (file.has_report ? file.report.cues_total : 0)
              << '\t' << (file.has_report ? file.report.cues_ok : 0)
              << '\t' << (file.has_report ? file.report.overflow_total : 0)
              << '\t' << (file.has_report ? file.report.lines_total : 0)
              << '\t' << (file.has_report ? format_fixed3(file.report.max_line_width26) : "0.000")
              << '\t' << (file.has_report ? file.report.invalid_utf8_total : 0)
              << '\t' << (file.has_report ? file.report.report_hash : "") << '\t'
              << file.error << '\n';
  }
  const std::string batch_tsv_path = (fs::path(out_dir) / "batch.tsv").string();
  if (!write_text_file(batch_tsv_path, batch_tsv.str(), error)) return fail(kExitInput, error);
  std::cout << "batch: files=" << files.size() << " failed=" << failed
            << " overflow=" << std::count_if(files.begin(), files.end(), [](const BatchFile& f) {
                 return f.has_report && f.report.overflow_total > 0;
               })
            << " hash=" << hashbuf << '\n';
  if (failed > 0) return kExitInput;
  return any_overflow ? kExitOverflow : kExitOk;
}

int command_selftest(const Args& args) {
  std::string error;
  if (!validate_keys(args, {}, error)) return fail(kExitUsage, error);
  const SelfTestResult result = run_selftest();
  if (!result.ok) {
    std::cerr << "selftest: FAIL: " << result.message << '\n';
    return kExitSelfTest;
  }
  std::cout << "selftest: OK " << result.message << '\n';
  return kExitOk;
}
}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    print_usage();
    return kExitUsage;
  }
  const std::string command = argv[1];
  if (command == "--help" || command == "-h" || command == "help") {
    print_usage();
    return kExitOk;
  }
  Args args;
  std::string error;
  if (!parse_args(argc, argv, args, error)) return fail(kExitUsage, error);
  if (command == "genfont") return command_genfont(args);
  if (command == "measure") return command_measure(args);
  if (command == "batch") return command_batch(args);
  if (command == "selftest") return command_selftest(args);
  return fail(kExitUsage, "unknown command: " + command);
}

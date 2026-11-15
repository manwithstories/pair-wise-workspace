#include "types.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iostream>
#include <map>
#include <sstream>
#include <string>
#include <vector>

using namespace sublay;

// exit codes: 0 ok, 2 param/parse failure, 3 font unusable,
//             4 overflow or break mismatch detected, 5 write failure

static void usage() {
  std::cerr <<
    "sublay - offline subtitle layout measurer\n"
    "  sublay measure -f a.ttf [-F b.ttf] [-s 48] [-w 1920] [-S 160]\n"
    "                 [-m 1..4] [-j jobs] -i in.subtxt -o out.json\n"
    "  sublay export  -i summary.json -o lines.jsonl\n"
    "  sublay verify  -s summary.json -e lines.jsonl\n"
    "  sublay selftest\n";
}

static bool read_all(const std::string& path, std::string& out) {
  std::ifstream f(path, std::ios::binary);
  if (!f) return false;
  std::stringstream ss;
  ss << f.rdbuf();
  out = ss.str();
  return true;
}

static int cmd_measure(int argc, char** argv) {
  std::vector<std::string> fonts;
  std::string input, output;
  LayoutParams params;
  int jobs = 1;
  for (int i = 0; i < argc; ++i) {
    std::string a = argv[i];
    auto next = [&](const char* flag) -> const char* {
      if (i + 1 >= argc) {
        std::cerr << "missing value for " << flag << "\n";
        exit(2);
      }
      return argv[++i];
    };
    if (a == "-f" || a == "-F") fonts.push_back(next("-f"));
    else if (a == "-s") params.size_px = atoi(next("-s"));
    else if (a == "-w") params.width = atoi(next("-w"));
    else if (a == "-S") params.safe = atoi(next("-S"));
    else if (a == "-m") params.max_lines = atoi(next("-m"));
    else if (a == "-j") jobs = atoi(next("-j"));
    else if (a == "-i") input = next("-i");
    else if (a == "-o") output = next("-o");
    else { std::cerr << "unknown arg: " << a << "\n"; return 2; }
  }
  if (fonts.empty() || input.empty() || output.empty()) {
    std::cerr << "measure requires -f, -i and -o\n";
    return 2;
  }
  if (params.size_px <= 0) {
    std::cerr << "font size must be > 0\n";
    return 2;
  }
  if (params.max_lines < 1 || params.max_lines > 4) {
    std::cerr << "max lines must be 1..4\n";
    return 2;
  }
  if (jobs < 1) jobs = 1;

  ParseResult pr = parse_subtxt(input);
  for (const auto& iss : pr.issues)
    std::cerr << input << ":" << iss.line << ":" << iss.col << ": "
              << iss.message << "\n";

  FontChain chain;
  std::string err;
  if (!chain.load(fonts, params.size_px, err)) {
    std::cerr << err << "\n";
    return 3;
  }
  MeasureOutput mo = measure_all(pr.cues, chain, params, jobs);
  std::string json = summary_json(mo, params, fonts);
  if (!write_file(output, json, err)) {
    std::cerr << err << "\n";
    return 5;
  }
  std::cout << "cues=" << mo.stats.cues
            << " glyphs=" << mo.stats.glyphs
            << " breaks=" << mo.stats.breaks
            << " overflow_count=" << mo.stats.overflow_count
            << " missing_glyph=" << mo.stats.missing_glyph
            << " skipped=" << pr.issues.size() << "\n";
  return pr.issues.empty() ? 0 : 2;
}

static int cmd_export(int argc, char** argv) {
  std::string input, output;
  for (int i = 0; i < argc; ++i) {
    std::string a = argv[i];
    if (a == "-i" && i + 1 < argc) input = argv[++i];
    else if (a == "-o" && i + 1 < argc) output = argv[++i];
    else { std::cerr << "unknown arg: " << a << "\n"; return 2; }
  }
  if (input.empty() || output.empty()) {
    std::cerr << "export requires -i and -o\n";
    return 2;
  }
  std::string json;
  if (!read_all(input, json)) {
    std::cerr << "cannot read: " << input << "\n";
    return 2;
  }
  std::vector<CueRecord> recs;
  if (!json_read_cues(json, recs)) {
    std::cerr << "bad summary json: " << input << "\n";
    return 2;
  }
  std::string err;
  if (!write_file(output, jsonl_from_records(recs), err)) {
    std::cerr << err << "\n";
    return 5;
  }
  std::cout << "exported=" << recs.size() << "\n";
  return 0;
}

static int cmd_verify(int argc, char** argv) {
  std::string sum, exp;
  for (int i = 0; i < argc; ++i) {
    std::string a = argv[i];
    if (a == "-s" && i + 1 < argc) sum = argv[++i];
    else if (a == "-e" && i + 1 < argc) exp = argv[++i];
    else { std::cerr << "unknown arg: " << a << "\n"; return 2; }
  }
  if (sum.empty() || exp.empty()) {
    std::cerr << "verify requires -s and -e\n";
    return 2;
  }
  std::string sj, ej;
  if (!read_all(sum, sj) || !read_all(exp, ej)) {
    std::cerr << "cannot read input\n";
    return 2;
  }
  std::vector<CueRecord> a, b;
  if (!json_read_cues(sj, a) || !json_read_cues(ej, b)) {
    std::cerr << "bad json input\n";
    return 2;
  }
  std::map<std::string, std::vector<size_t>> am;
  for (auto& r : a) am[r.cue_id] = r.breaks;
  long diffs = 0;
  for (auto& r : b) {
    auto it = am.find(r.cue_id);
    if (it == am.end() || it->second != r.breaks) {
      ++diffs;
      std::cerr << "break mismatch: " << r.cue_id << "\n";
    }
  }
  if (a.size() != b.size()) {
    std::cerr << "cue count differs: " << a.size() << " vs " << b.size() << "\n";
    ++diffs;
  }
  long overflow_count = 0;
  json_get_int(sj, "overflow_count", overflow_count);
  std::cout << "diffs=" << diffs << " overflow_count=" << overflow_count << "\n";
  return (diffs == 0 && overflow_count == 0) ? 0 : 4;
}

static int selftest() {
  int fails = 0;
  auto check = [&](bool ok, const char* name) {
    if (!ok) {
      ++fails;
      std::cerr << "FAIL " << name << "\n";
    }
  };
  {  // utf8 validation
    std::vector<UChar> u;
    size_t col = 0;
    check(utf8_decode("a\xe4\xb8\x82z", u, col) && u.size() == 3, "utf8 ok");
    std::string bad = "a\xc0\xaf";  // overlong
    check(!utf8_decode(bad, u, col) && col == 1, "utf8 overlong");
    bad = "x\xed\xa0\x80";  // surrogate
    check(!utf8_decode(bad, u, col), "utf8 surrogate");
  }
  {  // clusters: combining + ZWJ emoji + VS
    std::vector<UChar> u;
    size_t col;
    std::string s = "e\xcc\x81\xf0\x9f\x91\xa8\xe2\x80\x8d\xf0\x9f\x91\xa9";
    check(utf8_decode(s, u, col), "cluster decode");
    auto cl = make_clusters(u, s.size());
    check(cl.size() == 2, "combining+zwj clusters");
  }
  {  // break: forced break + empty text + zero safe width
    std::vector<ClusterInfo> ci(4);
    for (size_t i = 0; i < 4; ++i) {
      ci[i].byte_off = i;
      ci[i].byte_len = 1;
      ci[i].width = ft_from_int(10);
      ci[i].break_after = true;
    }
    auto r = layout_lines(ci, {2}, ft_from_int(100), 4);
    check(r.line_start.size() == 2 && r.breaks.size() == 1 && r.breaks[0] == 2,
          "forced break");
    r = layout_lines({}, {}, ft_from_int(100), 2);
    check(r.line_start.empty() && !r.overflow, "empty text");
    r = layout_lines(ci, {}, 0, 4);
    check(r.overflow && r.overflow_px > 0, "zero safe width");
    r = layout_lines(ci, {1, 1, 2}, ft_from_int(100), 2);
    check(r.overflow && r.line_start.size() == 4, "max_lines exceeded");
    for (auto& c : ci) c.width = ft_from_int(60);
    r = layout_lines(ci, {}, ft_from_int(100), 2);
    check(r.overflow && r.overflow_px == ft_from_int(20), "overflow px");
  }
  if (fails == 0) std::cout << "selftest ok\n";
  return fails ? 1 : 0;
}

int main(int argc, char** argv) {
  if (argc < 2) {
    usage();
    return 2;
  }
  std::string cmd = argv[1];
  if (cmd == "measure") return cmd_measure(argc - 2, argv + 2);
  if (cmd == "export") return cmd_export(argc - 2, argv + 2);
  if (cmd == "verify") return cmd_verify(argc - 2, argv + 2);
  if (cmd == "selftest") return selftest();
  usage();
  return 2;
}

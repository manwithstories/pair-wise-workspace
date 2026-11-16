#include <catch_amalgamated.hpp>

#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <sstream>
#include <string>

#include <sys/wait.h>

// End-to-end checks against the real executable. Each test runs `lutgrade` in a
// throwaway directory and asserts on exit code plus KEY=VALUE output, which is
// exactly the contract the acceptance criteria describe.

namespace {

struct RunResult {
  int exit_code = -1;
  std::string out;
};

std::string binary_path() {
#ifdef LUTGRADE_BINARY
  return LUTGRADE_BINARY;
#else
  return "./lutgrade";
#endif
}

RunResult run_cli(const std::string& args) {
  const std::string cmd = binary_path() + " " + args + " 2>&1";
  FILE* pipe = ::popen(cmd.c_str(), "r");
  RunResult result;
  if (pipe == nullptr) return result;
  char buffer[4096];
  while (std::fgets(buffer, sizeof(buffer), pipe) != nullptr) result.out += buffer;
  const int status = ::pclose(pipe);
  if (WIFEXITED(status)) result.exit_code = WEXITSTATUS(status);
  return result;
}

std::string temp_dir(const std::string& name) {
  return ".lutgrade_cli/" + name;
}

void reset_dir(const std::string& dir) {
  const std::string cmd = "rm -rf '" + dir + "' && mkdir -p '" + dir + "'";
  (void)std::system(cmd.c_str());
}

bool contains(const std::string& haystack, const std::string& needle) {
  return haystack.find(needle) != std::string::npos;
}

double value_of(const std::string& out, const std::string& key) {
  const std::string prefix = key + "=";
  const std::size_t pos = out.find(prefix);
  if (pos == std::string::npos) return -1.0;
  const std::size_t start = pos + prefix.size();
  const std::size_t end = out.find('\n', start);
  return std::stod(out.substr(start, end - start));
}

std::string string_value_of(const std::string& out, const std::string& key) {
  const std::string prefix = key + "=";
  const std::size_t pos = out.find(prefix);
  if (pos == std::string::npos) return "";
  const std::size_t start = pos + prefix.size();
  const std::size_t end = out.find('\n', start);
  return out.substr(start, end - start);
}

std::size_t count_bins(const std::string& out, const std::string& key) {
  const std::string line = string_value_of(out, key);
  std::size_t bins = 1;
  for (const char c : line) {
    if (c == ',') ++bins;
  }
  return line.empty() ? 0 : bins;
}

// Creates fixtures once for the whole executable run.
struct FixtureOnce {
  FixtureOnce() {
    (void)std::system("mkdir -p .lutgrade_cli");
    reset_dir(temp_dir("fix"));
    (void)run_cli("generate-fixture --dir " + temp_dir("fix") + " --size 33");
  }
};

const FixtureOnce& fixtures() {
  static FixtureOnce once;
  return once;
}

}  // namespace

TEST_CASE("cli: generate-fixture writes deterministic inputs") {
  const auto r = run_cli("generate-fixture --dir " + temp_dir("gen") + " --size 33");
  REQUIRE(r.exit_code == 0);
  REQUIRE(contains(r.out, "GRID_SIZE=33"));
  REQUIRE(contains(r.out, "PIXELS=4096"));
  std::ifstream ppm(temp_dir("gen") + "/input.ppm", std::ios::binary);
  REQUIRE(ppm.good());
  std::ifstream cube(temp_dir("gen") + "/demo.cube");
  REQUIRE(cube.good());
  std::ifstream lut17(temp_dir("gen") + "/lut17.cube");
  REQUIRE(lut17.good());
}

TEST_CASE("cli: run produces the acceptance report") {
  const auto& f = fixtures();
  (void)f;
  const auto r = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                         temp_dir("fix") + "/demo.cube --in-space srgb --out-space rec709 " +
                         "--path tiled --threads 4 --output " + temp_dir("fix") + "/out.ppm");
  REQUIRE(r.exit_code == 0);
  REQUIRE(string_value_of(r.out, "GRID_SIZE") == "33");
  REQUIRE(string_value_of(r.out, "PIXELS") == "4096");
  REQUIRE(string_value_of(r.out, "PATH") == "tiled");
  REQUIRE(string_value_of(r.out, "THREADS") == "4");
  REQUIRE(string_value_of(r.out, "SATURATED_PIXELS") == "0");
  REQUIRE(string_value_of(r.out, "HASH").size() == 16);

  REQUIRE(value_of(r.out, "MAX_DEV_R") >= 0.0);
  REQUIRE(value_of(r.out, "MAX_DEV_R") <= 0.0001);
  REQUIRE(value_of(r.out, "MAX_DEV_G") <= 0.0001);
  REQUIRE(value_of(r.out, "MAX_DEV_B") <= 0.0001);
  REQUIRE(value_of(r.out, "MEAN_DEV") <= value_of(r.out, "MAX_DEV_R"));

  for (const char* key : {"HIST_BEFORE_R", "HIST_AFTER_R", "HIST_BEFORE_G",
                          "HIST_AFTER_G", "HIST_BEFORE_B", "HIST_AFTER_B"}) {
    CAPTURE(key);
    REQUIRE(count_bins(r.out, key) == 256);
  }
  std::ifstream out(temp_dir("fix") + "/out.ppm", std::ios::binary);
  REQUIRE(out.good());
}

TEST_CASE("cli: scalar and tiled runs report identical hashes") {
  const auto scalar = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                              temp_dir("fix") + "/demo.cube --path scalar --output " +
                              temp_dir("fix") + "/s.ppm");
  const auto tiled = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                             temp_dir("fix") + "/demo.cube --path tiled --threads 8 --output " +
                             temp_dir("fix") + "/t.ppm");
  REQUIRE(scalar.exit_code == 0);
  REQUIRE(tiled.exit_code == 0);
  REQUIRE(string_value_of(scalar.out, "HASH") == string_value_of(tiled.out, "HASH"));
}

TEST_CASE("cli: compare matches and reports both hashes") {
  const auto r = run_cli("compare --input " + temp_dir("fix") + "/input.ppm --lut " +
                         temp_dir("fix") + "/demo.cube --in-space srgb --out-space rec709 " +
                         "--threads 4");
  REQUIRE(r.exit_code == 0);
  REQUIRE(contains(r.out, "COMPARE_RESULT=MATCH"));
  REQUIRE(string_value_of(r.out, "HASH_SCALAR") == string_value_of(r.out, "HASH_TILED"));
  REQUIRE(value_of(r.out, "MAX_DIFF") == 0.0);
}

TEST_CASE("cli: bad arguments exit 2 with a named error") {
  const auto missing = run_cli("run --input nope.ppm --lut nope.cube --output x.ppm");
  REQUIRE(missing.exit_code == 2);
  REQUIRE(contains(missing.out, "file_not_found"));

  const auto bad_path = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                                temp_dir("fix") + "/demo.cube --path diagonal --output " +
                                temp_dir("fix") + "/x.ppm");
  REQUIRE(bad_path.exit_code == 2);
  REQUIRE(contains(bad_path.out, "path_invalid"));

  const auto bad_threads = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                                   temp_dir("fix") + "/demo.cube --path tiled --threads 0 " +
                                   "--output " + temp_dir("fix") + "/x.ppm");
  REQUIRE(bad_threads.exit_code == 2);
  REQUIRE(contains(bad_threads.out, "threads_invalid"));
}

TEST_CASE("cli: unknown colour space exits 3") {
  const auto r = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " +
                         temp_dir("fix") + "/demo.cube --in-space adobe --output " +
                         temp_dir("fix") + "/x.ppm");
  REQUIRE(r.exit_code == 3);
  REQUIRE(contains(r.out, "space_unknown"));
}

TEST_CASE("cli: malformed LUT exits 2 and writes nothing") {
  const std::string bad = temp_dir("fix") + "/bad.cube";
  {
    std::ofstream f(bad);
    f << "LUT_3D_SIZE 33\n1 0 0\n";
  }
  const std::string out = temp_dir("fix") + "/never.ppm";
  (void)std::remove(out.c_str());
  const auto r = run_cli("run --input " + temp_dir("fix") + "/input.ppm --lut " + bad +
                         " --output " + out);
  REQUIRE(r.exit_code == 2);
  REQUIRE(contains(r.out, "cube_invalid"));
  std::ifstream check(out, std::ios::binary);
  REQUIRE_FALSE(check.good());
}

TEST_CASE("cli: usage errors from CLI11 map to exit 2") {
  const auto r = run_cli("run --input");
  REQUIRE(r.exit_code == 2);
}

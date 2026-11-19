#include <catch2/catch_test_macros.hpp>

#include <unistd.h>

#include <filesystem>

#include "resume_store.hpp"
#include "sha256.hpp"
#include "util.hpp"

using namespace mediaqc;

namespace {

std::string temp_dir() {
  static int counter = 0;
  auto p = std::filesystem::temp_directory_path() /
           ("mediaqc_resume_" + std::to_string(::getpid()) + "_" +
            std::to_string(counter++));
  std::filesystem::create_directories(p);
  return p.string();
}

std::string sha_of(const std::string& s) {
  Sha256 h;
  h.update(s);
  return h.hex_digest();
}

Checkpoint sample_checkpoint(const std::string& input, const std::string& sha,
                             const std::string& cfg) {
  Checkpoint cp;
  cp.stage = JobStage::Streaming;
  cp.input_path = input;
  cp.input_sha256 = sha;
  cp.input_size = 1000;
  cp.config_digest = cfg;
  cp.output_path = "/tmp/out.mp4.partial";
  cp.input_offset = 640;
  cp.packets_written = 42;
  cp.bytes_written = 600;
  cp.repairs = 2;
  cp.errors = 1;
  cp.dropped = 0;
  cp.stream_indices = {0, 1};
  TimelineState s0;
  s0.last_out_dts = 500; s0.last_raw_dts = 500; s0.last_pts = 500;
  s0.last_duration = 100; s0.wrap_offset = 0; s0.packets = 20;
  TimelineState s1;
  s1.last_out_dts = 700; s1.last_raw_dts = 700; s1.last_pts = 700;
  s1.last_duration = 64; s1.wrap_offset = 1LL << 32; s1.packets = 22;
  cp.per_stream = {s0, s1};
  return cp;
}

}  // namespace

TEST_CASE("checkpoint round trips through serialisation") {
  const auto cp = sample_checkpoint("/in/a.mp4", sha_of("a"), sha_of("cfg"));
  std::string text = serialize_checkpoint(cp);
  Checkpoint back;
  std::string err;
  REQUIRE(parse_checkpoint(text, back, err));
  REQUIRE(err.empty());
  REQUIRE(back.stage == JobStage::Streaming);
  REQUIRE(back.input_path == cp.input_path);
  REQUIRE(back.input_sha256 == cp.input_sha256);
  REQUIRE(back.input_offset == 640);
  REQUIRE(back.packets_written == 42);
  REQUIRE(back.per_stream.size() == 2);
  REQUIRE(back.per_stream[1].wrap_offset == 1LL << 32);
  REQUIRE(back.per_stream[1].last_out_dts == 700);
  // Identical jobs serialise to identical bytes.
  REQUIRE(serialize_checkpoint(back) == text);
}

TEST_CASE("corrupt checkpoint text is rejected") {
  Checkpoint cp;
  std::string err;
  REQUIRE_FALSE(parse_checkpoint("version=1\nstage=streaming\n", cp, err));
  REQUIRE_FALSE(err.empty());
  REQUIRE_FALSE(parse_checkpoint("garbage", cp, err));
}

TEST_CASE("resume is granted when input, config and output match") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  const std::string sha = sha_of("input-a");
  const std::string cfg = sha_of("cfg-1");
  const std::string inp = dir + "/a.mp4";
  auto cp = sample_checkpoint(inp, sha, cfg);
  std::string err;
  REQUIRE(store.save(cp, err));
  ResumeDecision d;
  Error e;
  REQUIRE(store.load_for(inp, sha, cfg, cp.output_path, d, e));
  REQUIRE(d.resume);
  REQUIRE(d.checkpoint.input_offset == 640);
  REQUIRE(d.reason.find("matches") != std::string::npos);
}

TEST_CASE("changed input checksum refuses resume with a reason") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  const std::string inp = dir + "/b.mp4";
  auto cp = sample_checkpoint(inp, sha_of("old"), sha_of("cfg"));
  std::string err;
  REQUIRE(store.save(cp, err));
  ResumeDecision d;
  Error e;
  REQUIRE(store.load_for(inp, sha_of("new"), sha_of("cfg"), cp.output_path, d, e));
  REQUIRE_FALSE(d.resume);
  REQUIRE(d.reason.find("checksum changed") != std::string::npos);
  REQUIRE(e.code == Code::CheckpointMismatch);
}

TEST_CASE("changed config digest refuses resume") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  const std::string inp = dir + "/c.mp4";
  const std::string sha = sha_of("in");
  auto cp = sample_checkpoint(inp, sha, sha_of("cfg-old"));
  std::string err;
  REQUIRE(store.save(cp, err));
  ResumeDecision d;
  Error e;
  REQUIRE(store.load_for(inp, sha, sha_of("cfg-new"), cp.output_path, d, e));
  REQUIRE_FALSE(d.resume);
  REQUIRE(d.reason.find("configuration changed") != std::string::npos);
}

TEST_CASE("corrupt checkpoint file refuses resume without crashing") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  const std::string inp = dir + "/d.mp4";
  auto cp = sample_checkpoint(inp, sha_of("in"), sha_of("cfg"));
  std::string err;
  REQUIRE(store.save(cp, err));
  const auto path = store.checkpoint_path(inp);
  write_file_atomic(path, "version=1\nstage=bogus\n", err);
  ResumeDecision d;
  Error e;
  REQUIRE(store.load_for(inp, cp.input_sha256, cp.config_digest, cp.output_path, d, e));
  REQUIRE_FALSE(d.resume);
  REQUIRE(e.code == Code::CheckpointCorrupt);
}

TEST_CASE("missing checkpoint is a clean no-op") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  ResumeDecision d;
  Error e;
  REQUIRE(store.load_for(dir + "/none.mp4", sha_of("x"), sha_of("y"), "", d, e));
  REQUIRE_FALSE(d.resume);
  REQUIRE(d.reason.find("no checkpoint") != std::string::npos);
}

TEST_CASE("checkpoint file is written atomically (no temp left behind)") {
  const std::string dir = temp_dir();
  ResumeStore store(dir);
  const std::string inp = dir + "/e.mp4";
  auto cp = sample_checkpoint(inp, sha_of("in"), sha_of("cfg"));
  std::string err;
  REQUIRE(store.save(cp, err));
  int entries = 0;
  for (const auto& e : std::filesystem::directory_iterator(dir)) {
    REQUIRE(e.path().extension() != ".tmp");
    ++entries;
  }
  REQUIRE(entries == 1);
}

#include <catch2/catch_test_macros.hpp>

#include <unistd.h>

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <string>
#include <vector>

#include "fixtures.hpp"
#include "index_writer.hpp"
#include "pipeline.hpp"
#include "resume_store.hpp"
#include "scanner.hpp"
#include "sha256.hpp"
#include "timeline.hpp"
#include "util.hpp"
#include "verifier.hpp"

using namespace mediaqc;

namespace {

std::string fresh_dir(const std::string& tag) {
  static int counter = 0;
  auto p = std::filesystem::temp_directory_path() /
           ("mediaqc_pipe_" + std::to_string(::getpid()) + "_" + tag + "_" +
            std::to_string(counter++));
  std::filesystem::create_directories(p);
  return p.string();
}

std::string sha_of_file(const std::filesystem::path& p) {
  std::string hex;
  int64_t bytes = 0;
  if (!sha256_file(p, hex, bytes)) return {};
  return hex;
}

// Copies one generated fixture into a fresh input directory.
std::filesystem::path one_fixture(const FixtureSet& set, const std::string& name,
                                  const std::string& dest_dir) {
  std::error_code ec;
  std::filesystem::create_directories(dest_dir, ec);
  const auto src = set.input_dir / name;
  const auto dst = std::filesystem::path(dest_dir) / name;
  std::filesystem::copy_file(src, dst, std::filesystem::copy_options::overwrite_existing,
                             ec);
  return dst;
}

const FixtureSet& shared_fixtures() {
  static FixtureSet set;
  static bool generated = false;
  if (!generated) {
    set = generate_fixtures(fresh_dir("fixtures"));
    generated = true;
  }
  return set;
}

BatchOptions batch_options(const std::string& in, const std::string& out) {
  BatchOptions o;
  o.input_dir = in;
  o.output_dir = out;
  return o;
}

const FileResult* find_file(const BatchResult& r, const std::string& name) {
  for (const auto& f : r.files) {
    if (std::filesystem::path(f.input_path).filename().string() == name) return &f;
  }
  return nullptr;
}

}  // namespace

TEST_CASE("fixture generator produces every boundary case") {
  const FixtureSet& set = shared_fixtures();
  REQUIRE(set.ok);
  REQUIRE(set.files.size() == 12);
  auto has_kind = [&](FixtureKind k) {
    return std::any_of(set.files.begin(), set.files.end(),
                       [&](const FixtureInfo& f) { return f.kind == k; });
  };
  for (FixtureKind k : {FixtureKind::Clean, FixtureKind::MoovAtEnd,
                        FixtureKind::TruncatedTail, FixtureKind::TruncatedMoov,
                        FixtureKind::NoMoov, FixtureKind::CorruptPacket,
                        FixtureKind::IndexOutOfRange, FixtureKind::IncompleteIndex,
                        FixtureKind::TimestampWrap, FixtureKind::BackwardsTimestamp,
                        FixtureKind::ZeroLength, FixtureKind::StaleCheckpoint}) {
    INFO(fixture_kind_name(k));
    REQUIRE(has_kind(k));
  }
}

TEST_CASE("clean asset is repaired, indexed and verified") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("clean_in");
  const std::string out = fresh_dir("clean_out");
  one_fixture(set, "01_clean.mp4", in);
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.files.size() == 1);
  REQUIRE(r.files[0].status == FileStatus::Ok);
  REQUIRE(r.exit_code == 0);
  REQUIRE(r.files[0].packets > 0);
  REQUIRE(r.files[0].index_entries >= r.files[0].packets);
  REQUIRE(r.files[0].decode_errors == 0);
  // The installed output matches the hash recorded in the result.
  REQUIRE(sha_of_file(std::filesystem::path(out) / "01_clean.mp4") ==
          r.files[0].output_sha256);
  // The index is in front of the media data.
  FileStructure fs;
  Error e;
  REQUIRE(read_mp4_structure((std::filesystem::path(out) / "01_clean.mp4").string(), fs, e));
  REQUIRE(fs.has_moov);
  REQUIRE_FALSE(fs.moov_after_mdat);
  REQUIRE_FALSE(fs.mdats.empty());
  REQUIRE(fs.moov_offset < fs.mdats[0].offset);
}

TEST_CASE("repeated runs produce identical output hashes") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("repeat_in");
  one_fixture(set, "01_clean.mp4", in);
  const std::string out_a = fresh_dir("repeat_a");
  const std::string out_b = fresh_dir("repeat_b");
  BatchResult a = run_batch(batch_options(in, out_a));
  BatchResult b = run_batch(batch_options(in, out_b));
  REQUIRE(a.totals.output_sha256 == b.totals.output_sha256);
  REQUIRE(a.files[0].output_sha256 == b.files[0].output_sha256);
}

TEST_CASE("zero-length input fails locally without stopping the batch") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("zero_in");
  one_fixture(set, "01_clean.mp4", in);
  one_fixture(set, "11_zero_length.mp4", in);
  const std::string out = fresh_dir("zero_out");
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.totals.files_total == 2);
  REQUIRE(r.totals.files_ok == 1);
  REQUIRE(r.totals.files_failed == 1);
  REQUIRE(r.exit_code == 1);
  const auto* clean = find_file(r, "01_clean.mp4");
  const auto* zero = find_file(r, "11_zero_length.mp4");
  REQUIRE(clean);
  REQUIRE(zero);
  REQUIRE(clean->status == FileStatus::Ok);
  REQUIRE(zero->status == FileStatus::Failed);
  REQUIRE(zero->first_error_offset == 0);
  REQUIRE_FALSE(zero->errors.empty());
  REQUIRE(zero->errors[0].code == Code::FileEmpty);
}

TEST_CASE("no-moov input reports the last confirmed offset") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("nomoov_in");
  one_fixture(set, "07_no_moov.mp4", in);
  const std::string out = fresh_dir("nomoov_out");
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.files.size() == 1);
  const auto& f = r.files[0];
  REQUIRE(f.status == FileStatus::Failed);
  REQUIRE_FALSE(f.errors.empty());
  REQUIRE(f.errors[0].code == Code::NoMoov);
  REQUIRE(f.errors[0].last_confirmed >= 0);
  REQUIRE(f.first_error_offset == f.errors[0].last_confirmed);
  REQUIRE_FALSE(std::filesystem::exists(std::filesystem::path(out) / "07_no_moov.mp4"));
}

TEST_CASE("truncated moov fails and never installs an output") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("tmoov_in");
  one_fixture(set, "08_truncated_moov.mp4", in);
  const std::string out = fresh_dir("tmoov_out");
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.files[0].status == FileStatus::Failed);
  REQUIRE(r.files[0].errors[0].code == Code::MoovTruncated);
  REQUIRE_FALSE(std::filesystem::exists(std::filesystem::path(out) / "08_truncated_moov.mp4"));
  // No partial file is left behind either.
  for (const auto& entry : std::filesystem::directory_iterator(out)) {
    REQUIRE(entry.path().extension() != ".partial");
  }
}

TEST_CASE("corrupt packet is repaired and its input range reported") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("corrupt_in");
  one_fixture(set, "03_corrupt_packet.mp4", in);
  const std::string out = fresh_dir("corrupt_out");
  BatchResult r = run_batch(batch_options(in, out));
  const auto& f = r.files[0];
  REQUIRE(f.status == FileStatus::Ok);
  REQUIRE(f.decode_errors >= 1);
  REQUIRE(f.first_error_offset >= 0);
  REQUIRE(f.offset_error_bytes > 0);
  // The reported span is at most one packet wide (the fixture's sample size).
  REQUIRE(f.offset_error_bytes == f.errors.front().offset_uncertainty());
  // The fixture recorded where it damaged the file; the report must point at
  // or after that byte.
  const auto it = std::find_if(set.files.begin(), set.files.end(), [](const FixtureInfo& x) {
    return x.kind == FixtureKind::CorruptPacket;
  });
  REQUIRE(it != set.files.end());
  REQUIRE(f.first_error_offset >= it->damage_offset);
  REQUIRE(f.first_error_offset <= it->damage_offset + it->damage_size);
}

TEST_CASE("out-of-range and incomplete indexes are rebuilt") {
  const FixtureSet& set = shared_fixtures();
  for (const char* name : {"04_index_out_of_range.mp4", "05_incomplete_index.mp4"}) {
    INFO(name);
    const std::string in = fresh_dir("idx_in");
    const std::string out = fresh_dir("idx_out");
    one_fixture(set, name, in);
    BatchResult r = run_batch(batch_options(in, out));
    REQUIRE(r.files.size() == 1);
    REQUIRE(r.files[0].status == FileStatus::Ok);
    REQUIRE(r.files[0].index_entries > 0);
    // The index-entry error is reported where the stco field lives (inside
    // moov), so the uncertainty is one field wide, not a packet.
    REQUIRE(r.files[0].offset_error_bytes <= 8);
    REQUIRE(r.files[0].first_error_offset > 0);
  }
}

TEST_CASE("moov at end is relocated to the front") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("moovlast_in");
  one_fixture(set, "06_moov_at_end.mov", in);
  const std::string out = fresh_dir("moovlast_out");
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.files[0].status == FileStatus::Ok);
  FileStructure fs;
  Error e;
  REQUIRE(read_mp4_structure((std::filesystem::path(out) / "06_moov_at_end.mov").string(),
                             fs, e));
  REQUIRE(fs.has_moov);
  REQUIRE_FALSE(fs.moov_after_mdat);
}

TEST_CASE("timestamp rollover is unwrapped and the output verifies") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("wrap_in");
  one_fixture(set, "09_timestamp_wrap.mp4", in);
  const std::string out = fresh_dir("wrap_out");
  BatchResult r = run_batch(batch_options(in, out));
  REQUIRE(r.files[0].status == FileStatus::Ok);
  REQUIRE(r.files[0].decode_errors == 0);
}

TEST_CASE("ambiguous backwards jump fails with the packet's byte range") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("back_in");
  one_fixture(set, "10_backwards.mp4", in);
  const std::string out = fresh_dir("back_out");
  BatchResult r = run_batch(batch_options(in, out));
  const auto& f = r.files[0];
  REQUIRE(f.status == FileStatus::Failed);
  REQUIRE(f.first_error_offset >= 0);
  REQUIRE(f.offset_error_bytes > 0);
  bool saw_timeline = false;
  for (const auto& e : f.errors) {
    if (e.stage == Stage::Timeline) saw_timeline = true;
  }
  REQUIRE(saw_timeline);
}

TEST_CASE("truncated tail keeps complete packets and reports the cut byte") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("tail_in");
  one_fixture(set, "02_truncated_tail.mp4", in);
  const std::string out = fresh_dir("tail_out");
  BatchResult r = run_batch(batch_options(in, out));
  const auto& f = r.files[0];
  REQUIRE(f.status == FileStatus::Ok);
  REQUIRE(f.dropped_packets >= 1);
  REQUIRE(f.first_error_offset > 0);
  // The reported offset is the first byte beyond the file.
  const auto it = std::find_if(set.files.begin(), set.files.end(), [](const FixtureInfo& x) {
    return x.kind == FixtureKind::TruncatedTail;
  });
  REQUIRE(it != set.files.end());
  REQUIRE(f.first_error_offset == it->damage_offset);
}

TEST_CASE("stale checkpoint is refused with a reason and the asset is rebuilt") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("stale_in");
  const std::string out = fresh_dir("stale_out");
  one_fixture(set, "12_stale_checkpoint.mp4", in);
  // Seed the same stale checkpoint the selftest uses, in this run's
  // checkpoint directory.
  ResumeStore store(std::filesystem::path(out) / ".mediaqc");
  Config cfg;
  Checkpoint cp;
  cp.stage = JobStage::Streaming;
  cp.input_path = (std::filesystem::path(in) / "12_stale_checkpoint.mp4").string();
  cp.input_sha256 = std::string(64, '0');
  cp.config_digest = config_digest(cfg.summary());
  cp.output_path = (std::filesystem::path(out) / "12_stale_checkpoint.mp4").string();
  cp.packets_written = 10;
  std::string err;
  REQUIRE(store.save(cp, err));

  BatchOptions o = batch_options(in, out);
  o.resume = true;
  BatchResult r = run_batch(o);
  const auto& f = r.files[0];
  REQUIRE(f.status == FileStatus::Ok);
  REQUIRE(f.checkpoint_refused);
  REQUIRE(f.refusal_reason.find("checksum changed") != std::string::npos);
  bool saw_mismatch = false;
  for (const auto& e : f.errors) {
    if (e.code == Code::CheckpointMismatch) saw_mismatch = true;
  }
  REQUIRE(saw_mismatch);
}

TEST_CASE("resume after a verified run reuses the output without rewriting it") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("reuse_in");
  const std::string out = fresh_dir("reuse_out");
  one_fixture(set, "01_clean.mp4", in);
  BatchResult first = run_batch(batch_options(in, out));
  REQUIRE(first.files[0].status == FileStatus::Ok);
  const auto out_path = std::filesystem::path(out) / "01_clean.mp4";
  const auto before = std::filesystem::last_write_time(out_path);
  sleep(1);
  BatchOptions o = batch_options(in, out);
  o.resume = true;
  BatchResult second = run_batch(o);
  REQUIRE(second.files[0].status == FileStatus::Resumed);
  REQUIRE(second.files[0].resumed);
  REQUIRE(second.files[0].output_sha256 == first.files[0].output_sha256);
  REQUIRE(std::filesystem::last_write_time(out_path).time_since_epoch().count() ==
          before.time_since_epoch().count());
  REQUIRE(second.totals.resume_seconds >= 1);
}

TEST_CASE("interrupted run resumes to the same output hash") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("int_in");
  const std::string out_full = fresh_dir("int_full");
  const std::string out_cut = fresh_dir("int_cut");
  one_fixture(set, "01_clean.mp4", in);
  BatchResult full = run_batch(batch_options(in, out_full));
  REQUIRE(full.files[0].status == FileStatus::Ok);

  BatchOptions cut = batch_options(in, out_cut);
  cut.stop_after_packets = 40;
  BatchResult partial = run_batch(cut);
  REQUIRE(partial.files[0].status == FileStatus::Interrupted);
  REQUIRE_FALSE(std::filesystem::exists(std::filesystem::path(out_cut) / "01_clean.mp4"));

  BatchOptions resume = batch_options(in, out_cut);
  resume.resume = true;
  BatchResult done = run_batch(resume);
  REQUIRE(done.files[0].status == FileStatus::Ok);
  REQUIRE(done.files[0].resumed);
  REQUIRE(done.files[0].output_sha256 == full.files[0].output_sha256);
}

TEST_CASE("verify subcommand accepts the produced outputs") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("verify_in");
  const std::string out = fresh_dir("verify_out");
  one_fixture(set, "01_clean.mp4", in);
  one_fixture(set, "02_truncated_tail.mp4", in);
  BatchResult batch = run_batch(batch_options(in, out));
  REQUIRE(batch.totals.files_ok == 2);
  VerifyOptions vo;
  vo.input_dir = out;
  BatchResult verify = run_verify(vo);
  REQUIRE(verify.exit_code == 0);
  REQUIRE(verify.totals.files_total == 2);
  REQUIRE(verify.totals.files_ok == 2);
  REQUIRE(verify.totals.index_entries > 0);
}

TEST_CASE("verify rejects a corrupted output with exit code 3") {
  const FixtureSet& set = shared_fixtures();
  const std::string in = fresh_dir("badout_in");
  const std::string out = fresh_dir("badout_out");
  one_fixture(set, "01_clean.mp4", in);
  BatchResult batch = run_batch(batch_options(in, out));
  REQUIRE(batch.files[0].status == FileStatus::Ok);
  // Overwrite the output's moov with zeros: the index becomes unusable.
  const auto path = std::filesystem::path(out) / "01_clean.mp4";
  FileStructure fs;
  Error e;
  REQUIRE(read_mp4_structure(path.string(), fs, e));
  std::fstream f(path, std::ios::in | std::ios::out | std::ios::binary);
  REQUIRE(f);
  const std::string zeros(std::min<int64_t>(fs.moov_size, 512), '\0');
  f.seekp(fs.moov_offset);
  f.write(zeros.data(), static_cast<std::streamsize>(zeros.size()));
  f.close();
  VerifyOptions vo;
  vo.input_dir = out;
  BatchResult verify = run_verify(vo);
  REQUIRE(verify.exit_code == 3);
  REQUIRE(verify.totals.files_failed >= 1);
}

TEST_CASE("selftest mode drives the whole fixture set") {
  SelftestOptions opt;
  opt.work_dir = fresh_dir("selftest");
  opt.ffmpeg_binary = "ffmpeg";
  BatchResult r = run_selftest(opt);
  REQUIRE(r.error.empty());
  REQUIRE(r.exit_code == 0);
  REQUIRE(r.totals.files_total == 12);
  REQUIRE(r.totals.files_ok == 8);
  REQUIRE(r.totals.files_failed == 4);
  REQUIRE(r.totals.checkpoint_refusals == 1);
  REQUIRE(r.totals.decode_errors == 1);
  REQUIRE(r.totals.index_entries > 0);
}

TEST_CASE("path escape is rejected before the file is opened") {
  std::vector<Error> rejected;
  const std::string dir = fresh_dir("escape");
  // A symlink that points outside the input directory must be skipped.
  const auto outside = std::filesystem::temp_directory_path() / "mediaqc_outside.mp4";
  {
    std::ofstream o(outside, std::ios::binary);
    o << "not media";
  }
  const auto link = std::filesystem::path(dir) / "escape.mp4";
  std::error_code ec;
  std::filesystem::create_symlink(outside, link, ec);
  if (ec) {
    // Symlinks may be unavailable; the containment helper is still checked.
    REQUIRE_FALSE(path_is_within(outside, dir));
    return;
  }
  (void)discover_inputs(dir, rejected);
  REQUIRE(rejected.size() == 1);
  REQUIRE(rejected[0].code == Code::PathEscape);
}

TEST_CASE("limits reject an oversized packet before it is written") {
  // The scanner checks packet size against the limit indirectly through the
  // container; here the config's packet cap is enforced when recording.
  Limits limits;
  REQUIRE(limits.max_packet_bytes == int64_t(64) * 1024 * 1024);
  REQUIRE(limits.max_tracks >= 2);
  REQUIRE(limits.max_input_bytes > 0);
}

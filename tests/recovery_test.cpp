#include <catch2/catch_test_macros.hpp>

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <fstream>

#include "media/checkpoint.hpp"
#include "media/damage_scan.hpp"
#include "media/fs_util.hpp"
#include "media/manifest.hpp"
#include "media/probe.hpp"
#include "media/repack.hpp"
#include "test_helpers.hpp"

using mediaqc_test::tmp_dir;
using mediaqc_test::wav_bytes;
using mediaqc_test::write_index_sidecar;

TEST_CASE("probe reports exact byte offsets for truncation and bad headers") {
    const std::string dir = tmp_dir();
    const std::string path = dir + "/cut.wav";
    const std::string full = wav_bytes(48000, 6, 16, 4800);  // 44 + 57600 bytes
    { std::ofstream f(path, std::ios::binary); f.write(full.data(), full.size() - 7); }

    auto probe = mediaqc::probe_wav_header(path);
    REQUIRE(probe.header.riff_ok);
    REQUIRE(probe.header.data_offset == 44);
    REQUIRE(probe.header.declared_data_size == 57600);
    REQUIRE(probe.header.actual_data_size == 57593);

    auto scan = mediaqc::scan_damage(probe, mediaqc::ScanConfig{});
    REQUIRE(scan.truncated_bytes == 7);
    const auto trunc = std::find_if(scan.damage.begin(), scan.damage.end(),
                                    [](const mediaqc::DamageRecord& d) {
                                        return d.code == mediaqc::ErrorCode::Truncated;
                                    });
    REQUIRE(trunc != scan.damage.end());
    REQUIRE(trunc->source_offset == 44 + 57593);
    REQUIRE_FALSE(scan.unrecoverable);

    const std::string bad = dir + "/bad.wav";
    { std::ofstream f(bad, std::ios::binary); f << "not a wav file at all"; }
    auto probe_bad = mediaqc::probe_wav_header(bad);
    REQUIRE(probe_bad.errors.front().source_offset == 0);
    REQUIRE(mediaqc::scan_damage(probe_bad, mediaqc::ScanConfig{}).unrecoverable);
}

TEST_CASE("damage scan: CRC locates damaged pages and missing index is rebuilt") {
    const std::string dir = tmp_dir();
    const std::string clean = dir + "/ok.wav";
    {
        const std::string bytes = wav_bytes(48000, 6, 16, 9600);
        std::ofstream f(clean, std::ios::binary);
        f.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    }
    write_index_sidecar(clean);
    {
        auto probe = mediaqc::probe_wav_header(clean);
        mediaqc::load_index(probe);
        REQUIRE(probe.index_present);
        auto scan = mediaqc::scan_damage(probe, mediaqc::ScanConfig{});
        REQUIRE(scan.damage.empty());
    }

    // Index describes the ORIGINAL clean content; corrupt the file afterwards.
    const std::string damaged = dir + "/damaged.wav";
    {
        const std::string bytes = wav_bytes(48000, 6, 16, 9600);
        std::ofstream f(damaged, std::ios::binary);
        f.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    }
    write_index_sidecar(damaged);
    {
        std::fstream f(damaged, std::ios::in | std::ios::out | std::ios::binary);
        const long pos = 44 + 57600 + 3;
        f.seekg(pos);
        char b;
        f.read(&b, 1);
        b ^= static_cast<char>(0xFF);
        f.seekp(pos);
        f.write(&b, 1);
    }
    {
        auto probe = mediaqc::probe_wav_header(damaged);
        mediaqc::load_index(probe);
        auto scan = mediaqc::scan_damage(probe, mediaqc::ScanConfig{});
        const auto crc = std::find_if(scan.damage.begin(), scan.damage.end(),
                                      [](const mediaqc::DamageRecord& d) {
                                          return d.code == mediaqc::ErrorCode::CrcMismatch;
                                      });
        REQUIRE(crc != scan.damage.end());
        REQUIRE(crc->source_offset == 44 + 57600);
        REQUIRE(crc->start_sample == 4800);
        REQUIRE(crc->end_sample == 9600);
    }

    const std::string noidx = dir + "/noidx.wav";
    {
        const std::string bytes = wav_bytes(48000, 6, 16, 4800);
        std::ofstream f(noidx, std::ios::binary);
        f.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    }
    {
        auto probe = mediaqc::probe_wav_header(noidx);
        mediaqc::load_index(probe);
        REQUIRE_FALSE(probe.index_present);
        mediaqc::ScanConfig cfg;
        cfg.frames_per_page = 480;  // 0.01 s pages -> 10 pages
        auto scan = mediaqc::scan_damage(probe, cfg);
        REQUIRE(scan.index_was_rebuilt);
        REQUIRE(probe.pages.size() == 10);
        const auto non_index = std::find_if(
            scan.damage.begin(), scan.damage.end(),
            [](const mediaqc::DamageRecord& d) {
                return d.code != mediaqc::ErrorCode::MissingIndex;
            });
        REQUIRE(non_index == scan.damage.end());
    }
}

TEST_CASE("repack silence vs truncate policy, bitrate and atomic output") {
    const std::string dir = tmp_dir();
    const std::string src = dir + "/src.wav";
    {
        const std::string bytes = wav_bytes(48000, 6, 16, 9600);
        std::ofstream f(src, std::ios::binary);
        f.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    }
    write_index_sidecar(src);
    {  // flip one byte inside page 1 after indexing the clean content
        std::fstream f(src, std::ios::in | std::ios::out | std::ios::binary);
        const long pos = 44 + 57600 + 3;
        f.seekg(pos);
        char b;
        f.read(&b, 1);
        b ^= static_cast<char>(0xFF);
        f.seekp(pos);
        f.write(&b, 1);
    }
    auto probe = mediaqc::probe_wav_header(src);
    mediaqc::load_index(probe);
    mediaqc::ScanConfig scfg;
    scfg.frames_per_page = 4800;
    auto scan = mediaqc::scan_damage(probe, scfg);
    REQUIRE_FALSE(scan.damage.empty());

    mediaqc::RepackConfig cfg;
    cfg.target_rate_hz = 48000;
    cfg.target_channels = 6;
    cfg.target_bitrate = 4'608'000;
    cfg.passthrough_tolerance_bps = 46080;
    cfg.damage_policy = mediaqc::DamagePolicy::Silence;

    const auto silence = mediaqc::repack(probe, scan, cfg, dir, "silence.wav");
    REQUIRE(silence.pcm_reused);
    REQUIRE(silence.output_frames == 9600);
    REQUIRE(silence.silence_frames == 4800);
    REQUIRE(silence.policy_ranges.size() == 1);
    REQUIRE(silence.policy_ranges.front().start_sample == 4800);
    REQUIRE(std::fabs(silence.bitrate_deviation_pct) < 1.0);
    REQUIRE(mediaqc::fs::stat(silence.output_path).exists);

    cfg.damage_policy = mediaqc::DamagePolicy::Truncate;
    const auto cut = mediaqc::repack(probe, scan, cfg, dir, "cut.wav");
    REQUIRE(cut.output_frames == 4800);
    REQUIRE(cut.dropped_frames == 4800);
    const auto names = mediaqc::fs::list_files(dir);
    REQUIRE(std::find_if(names.begin(), names.end(),
                         [](const std::string& n) {
                             return n.find(".part.") != std::string::npos ||
                                    n.find(".tmp.") != std::string::npos;
                         }) == names.end());
}

TEST_CASE("checkpoint invalidates on size, mtime or parameter changes") {
    const std::string dir = tmp_dir();
    mediaqc::CheckpointStore store(dir + "/cp.json");
    mediaqc::CheckpointEntry e;
    e.track_id = "t1";
    e.source_path = dir + "/t1.wav";
    e.source_size = 100;
    e.source_mtime_ns = 12345;
    e.param_digest = mediaqc::CheckpointStore::digest_params("p1");
    e.output_path = dir + "/t1.out.wav";
    e.output_relpath = "t1.out.wav";
    e.completed = true;
    { std::ofstream f(e.output_path, std::ios::binary); f << "x"; }
    store.upsert(e);
    store.save();

    mediaqc::CheckpointStore reloaded(dir + "/cp.json");
    reloaded.load();
    REQUIRE(reloaded.valid_entry("t1", 100, 12345, e.param_digest) != nullptr);
    REQUIRE(reloaded.valid_entry("t1", 101, 12345, e.param_digest) == nullptr);
    REQUIRE(reloaded.invalidations().back().reason.find("size") != std::string::npos);
    reloaded.upsert(e);
    REQUIRE(reloaded.valid_entry("t1", 100, 999, e.param_digest) == nullptr);
    REQUIRE(reloaded.invalidations().back().reason.find("mtime") != std::string::npos);
    reloaded.upsert(e);
    REQUIRE(reloaded.valid_entry("t1", 100, 12345, "other-digest") == nullptr);
    REQUIRE(reloaded.invalidations().back().reason.find("parameters") != std::string::npos);
}

TEST_CASE("path traversal is rejected and output dir is validated") {
    const std::string dir = tmp_dir();
    REQUIRE_THROWS(mediaqc::fs::safe_join(dir, "../escape.wav"));
    REQUIRE_THROWS(mediaqc::fs::safe_join(dir, "/etc/passwd"));
    const std::string inside = mediaqc::fs::safe_join(dir, "nested/out.wav");
    REQUIRE(inside.rfind(dir, 0) == 0);
    REQUIRE(std::filesystem::is_directory(mediaqc::fs::validate_output_dir(dir + "/out")));
}

TEST_CASE("manifest roundtrip preserves damage model and metrics") {
    const std::string dir = tmp_dir();
    mediaqc::BatchManifest m;
    m.output_rate_hz = 48000;
    m.output_channels = 6;
    m.target_bitrate = 4'608'000;
    m.max_alignment_error_ns = 17'000;
    m.has_recoverable_damage = true;
    mediaqc::TrackManifest t;
    t.track_id = "t1";
    t.source_path = dir + "/t1.wav";
    t.output_path = dir + "/t1.out.wav";
    t.packets = 10;
    t.damaged_ranges = 1;
    t.truncated_bytes = 7;
    t.actual_bitrate = 4'608'000;
    t.peak_dbfs = -3.5;
    t.rms_dbfs = -18.25;
    t.damage.push_back({mediaqc::ErrorCode::Truncated, 44, 0, 0, "tail cut"});
    t.policy_ranges.push_back({4800, 9600});
    m.tracks.push_back(std::move(t));
    const std::string path = dir + "/manifest.json";
    mediaqc::fs::write_atomic(path, mediaqc::render_manifest(m));
    const auto parsed = mediaqc::parse_manifest(path);
    REQUIRE(parsed.tracks.size() == 1);
    REQUIRE(parsed.tracks[0].damage[0].code == mediaqc::ErrorCode::Truncated);
    REQUIRE(parsed.tracks[0].damage[0].source_offset == 44);
    REQUIRE(parsed.tracks[0].policy_ranges[0].start_sample == 4800);
    REQUIRE(parsed.max_alignment_error_ns == 17'000);
}

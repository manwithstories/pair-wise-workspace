#include "selftest.h"

#include "bit_reader.h"
#include "nal_split.h"
#include "param_sets.h"
#include "picture_order.h"

#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <sys/wait.h>
#include <vector>

namespace h264 {
namespace {

struct CommandResult {
    int exit_code = -1;
    std::string output;
};

std::string read_file(const std::filesystem::path& path) {
    std::ifstream in(path, std::ios::binary);
    std::ostringstream out;
    out << in.rdbuf();
    return out.str();
}

std::string quote(const std::string& value) {
    return "\"" + value + "\"";
}

CommandResult run_command(const std::string& executable, const std::string& args,
                          const std::filesystem::path& output_path) {
    const std::string command = quote(executable) + " " + args + " > " +
                                quote(output_path.string()) + " 2>&1";
    const int status = std::system(command.c_str());
    CommandResult result;
    if (WIFEXITED(status)) result.exit_code = WEXITSTATUS(status);
    result.output = read_file(output_path);
    return result;
}

std::size_t count_text(const std::string& text, const std::string& needle) {
    std::size_t count = 0;
    std::size_t position = 0;
    while ((position = text.find(needle, position)) != std::string::npos) {
        ++count;
        position += needle.size();
    }
    return count;
}

bool contains(const std::string& text, const std::string& needle) {
    return text.find(needle) != std::string::npos;
}

}  // namespace

int run_selftest(const std::string& executable, const std::string& fixtures_dir) {
    int failures = 0;
    auto expect = [&](bool condition, const std::string& message) {
        std::cout << (condition ? "[PASS] " : "[FAIL] ") << message << '\n';
        if (!condition) ++failures;
    };
    const std::uint8_t long_zero_code[] = {0, 0, 0, 0, 0x08};
    BitReader invalid(long_zero_code, sizeof(long_zero_code));
    invalid.ue();
    expect(!invalid.ok(), "Exp-Golomb rejects more than 32 leading zero bits");

    const std::filesystem::path fixtures(fixtures_dir);
    const auto nonce = std::chrono::steady_clock::now().time_since_epoch().count();
    const std::filesystem::path temp = std::filesystem::temp_directory_path() /
                                       ("h264_audit_selftest_" + std::to_string(nonce));
    std::filesystem::create_directories(temp);

    const std::vector<std::uint8_t> start_code_data = {
        0, 0, 1, 0x09, 0x55, 0xaa, 0, 0, 0, 1, 0x68, 0x66
    };
    {
        std::ofstream out(temp / "start_codes.h264", std::ios::binary);
        out.write(reinterpret_cast<const char*>(start_code_data.data()),
                  static_cast<std::streamsize>(start_code_data.size()));
    }
    std::vector<NalUnit> start_code_nals;
    std::uint64_t start_code_size = 0;
    std::string start_code_error;
    const bool scanned = scan_nal_units((temp / "start_codes.h264").string(), start_code_nals,
                                        start_code_size, start_code_error);
    expect(scanned && start_code_nals.size() == 2, "scanner accepts 3-byte and 4-byte start codes");
    expect(scanned && start_code_nals[0].offset == 3 && start_code_nals[0].size == 3 &&
               start_code_nals[1].start_code_offset == 6 && start_code_nals[1].offset == 10,
           "NAL offsets are absolute and exclude start codes");
    expect(scanned && !start_code_nals[0].start_code_4byte &&
               start_code_nals[1].start_code_4byte &&
               start_code_nals[1].type == 8 && start_code_nals[1].at_eof,
           "scanner handles consecutive zero bytes before a start code");

    SpsInfo type0_sps;
    type0_sps.log2_max_frame_num = 4;
    type0_sps.pic_order_cnt_type = 0;
    type0_sps.log2_max_pic_order_cnt_lsb = 4;
    PictureOrderCounter type0_counter;
    SliceInfo type0_slice;
    type0_slice.idr = true;
    type0_slice.nal_ref_idc = 3;
    PocInfo type0_poc;
    std::string poc_error;
    bool poc_ok = type0_counter.compute(type0_slice, type0_sps, type0_poc, poc_error) &&
                  type0_poc.poc == 0;
    type0_slice.idr = false;
    type0_slice.frame_num = 1;
    type0_slice.pic_order_cnt_lsb = 8;
    poc_ok = poc_ok && type0_counter.compute(type0_slice, type0_sps, type0_poc, poc_error) &&
             type0_poc.poc == 8;
    type0_slice.nal_ref_idc = 0;
    type0_slice.frame_num = 2;
    type0_slice.pic_order_cnt_lsb = 0;
    poc_ok = poc_ok && type0_counter.compute(type0_slice, type0_sps, type0_poc, poc_error) &&
             type0_poc.poc == 16;
    expect(poc_ok, "POC type 0 handles reference-only state and LSB wrap");

    SpsInfo type1_sps;
    type1_sps.log2_max_frame_num = 4;
    type1_sps.pic_order_cnt_type = 1;
    type1_sps.offset_for_non_ref_pic = -2;
    type1_sps.offset_for_top_to_bottom_field = 1;
    type1_sps.offset_for_ref_frame = {4, 4};
    PictureOrderCounter type1_counter;
    SliceInfo type1_slice;
    type1_slice.idr = true;
    type1_slice.nal_ref_idc = 3;
    poc_ok = type1_counter.compute(type1_slice, type1_sps, type0_poc, poc_error);
    type1_slice.idr = false;
    type1_slice.frame_num = 1;
    poc_ok = poc_ok && type1_counter.compute(type1_slice, type1_sps, type0_poc, poc_error) &&
             type0_poc.poc == 4 && type0_poc.bottom == 5;
    type1_slice.nal_ref_idc = 0;
    type1_slice.frame_num = 2;
    poc_ok = poc_ok && type1_counter.compute(type1_slice, type1_sps, type0_poc, poc_error) &&
             type0_poc.poc == 2;
    expect(poc_ok, "POC type 1 applies cycle offsets and non-reference offset");

    SpsInfo type2_sps;
    type2_sps.log2_max_frame_num = 4;
    type2_sps.pic_order_cnt_type = 2;
    PictureOrderCounter type2_counter;
    SliceInfo type2_slice;
    type2_slice.idr = true;
    type2_slice.nal_ref_idc = 3;
    poc_ok = type2_counter.compute(type2_slice, type2_sps, type0_poc, poc_error);
    type2_slice.idr = false;
    type2_slice.frame_num = 1;
    type2_slice.nal_ref_idc = 0;
    poc_ok = poc_ok && type2_counter.compute(type2_slice, type2_sps, type0_poc, poc_error) &&
             type0_poc.poc == 1;
    type2_slice.frame_num = 15;
    type2_slice.nal_ref_idc = 3;
    poc_ok = poc_ok && type2_counter.compute(type2_slice, type2_sps, type0_poc, poc_error) &&
             type0_poc.poc == 30;
    type2_slice.frame_num = 0;
    poc_ok = poc_ok && type2_counter.compute(type2_slice, type2_sps, type0_poc, poc_error) &&
             type0_poc.poc == 32 && type0_poc.frame_num_wrapped;
    expect(poc_ok, "POC type 2 handles non-reference and frame_num wrap");

    auto analyze = [&](const std::string& name, const std::string& args) {
        return run_command(executable, args, temp / (name + ".out"));
    };

    const std::string mixed = (fixtures / "mixed_1080p.h264").string();
    const std::string mixed_json = (temp / "mixed.json").string();
    const CommandResult mixed_result = analyze(
        "mixed", "scan " + quote(mixed) + " --json " + quote(mixed_json));
    expect(mixed_result.exit_code == 0, "mixed stream exits 0 without warnings");
    expect(contains(mixed_result.output, "profile_idc=100 level_idc=41 chroma_format_idc=1"),
           "mixed SPS profile, level and chroma");
    expect(contains(mixed_result.output, "bit_depth_luma=8 pic_order_cnt_type=0 1920x1080"),
           "mixed SPS bit depth, POC type and display size");
    expect(contains(mixed_result.output, "frames=240 I=8 P=176 B=56 slices_per_au_max=3"),
           "mixed frame totals and maximum slices per AU");
    expect(contains(mixed_result.output, "GOPs: count=8"), "mixed stream has eight GOPs");
    expect(contains(mixed_result.output, "IPBPBPBPBPBPBPBPPPPPPPPPPPPPPP"),
           "mixed frame type string follows POC display order");
    const std::string mixed_json_text = read_file(mixed_json);
    expect(count_text(mixed_json_text, "\"index\":") == 8, "JSON contains eight GOP entries");
    expect(contains(mixed_json_text, "\"sps\"") && contains(mixed_json_text, "\"pps\"") &&
               contains(mixed_json_text, "\"frames\"") && contains(mixed_json_text, "\"gops\"") &&
               contains(mixed_json_text, "\"warnings\""), "JSON has required top-level keys");
    const std::size_t sps_key = mixed_json_text.find("\"sps\"");
    const std::size_t pps_key = mixed_json_text.find("\"pps\"");
    const std::size_t frames_key = mixed_json_text.find("\"frames\"");
    const std::size_t gops_key = mixed_json_text.find("\"gops\"");
    const std::size_t warnings_key = mixed_json_text.find("\"warnings\"");
    expect(sps_key < pps_key && pps_key < frames_key && frames_key < gops_key &&
               gops_key < warnings_key, "JSON top-level key order is deterministic");
    const CommandResult strict_result = analyze("strict", "scan " + quote(mixed) + " --strict");
    expect(strict_result.exit_code == 0, "strict mixed stream is consistent");

    const CommandResult paff_result = analyze(
        "paff", "gops " + quote((fixtures / "interlaced_paff.h264").string()) +
                    " --recovery-point --min-gop 8");
    expect(paff_result.exit_code == 0, "PAFF stream with recovery points exits 0");
    expect(contains(paff_result.output, "frames=96"), "PAFF field pairs merge into 96 frames");
    expect(contains(paff_result.output, "GOPs: count=12"), "PAFF recovery boundaries honor min-gop");

    const CommandResult mbaff_result = analyze(
        "mbaff", "scan " + quote((fixtures / "interlaced_mbaff.h264").string()));
    expect(mbaff_result.exit_code == 0, "MBAFF stream exits 0");
    expect(contains(mbaff_result.output, "mb_adaptive_frame_field_flag=1"),
           "MBAFF flag is reported");
    expect(contains(mbaff_result.output, "frames=64"), "MBAFF frame-coded pictures stay 64 frames");

    const CommandResult long_result = analyze(
        "long", "gops " + quote((fixtures / "long_gop.h264").string()) +
                    " --recovery-point --min-gop 8");
    expect(long_result.exit_code == 0, "long GOP recovery stream exits 0");
    expect(contains(long_result.output, "frames=300"), "long GOP stream has 300 frames");

    const CommandResult truncated_result = analyze(
        "truncated", "scan " + quote((fixtures / "truncated.h264").string()));
    expect(truncated_result.exit_code == 2, "truncated tail returns warning exit code 2");
    expect(contains(truncated_result.output, "frames=30"), "truncated tail keeps prior 30 frames");
    expect(contains(truncated_result.output, "truncated slice"), "truncated tail is reported");

    std::filesystem::remove_all(temp);
    if (failures != 0) {
        std::cerr << "selftest failed: " << failures << " assertion(s)\n";
        return 3;
    }
    std::cout << "selftest passed\n";
    return 0;
}

}  // namespace h264

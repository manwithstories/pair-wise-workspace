#include <cmath>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <iterator>
#include <sstream>
#include <stdexcept>
#include <string>
#include <vector>

#include "color_matrix.h"
#include "cube_parser.h"
#include "histogram.h"
#include "image_io.h"
#include "kernel.h"

namespace {

using namespace lutgrade;

void check(bool condition, const std::string& message) {
  if (!condition) throw std::runtime_error(message);
}

std::string quote(const std::string& value) {
  std::string result = "'";
  for (const char ch : value) {
    if (ch == '\'') result += "'\\''";
    else result.push_back(ch);
  }
  return result + "'";
}

int run_command(const std::string& command) {
  const int status = std::system(command.c_str());
  if (status == -1) return -1;
#ifdef _WIN32
  return status;
#else
  return (status >> 8) & 0xFF;
#endif
}

int run_cli(const std::string& executable, const std::string& arguments,
            const std::filesystem::path& stdout_path = {}) {
  std::string command = quote(executable) + " " + arguments;
  if (!stdout_path.empty()) command += " > " + quote(stdout_path.string()) + " 2>&1";
  return run_command(command);
}

std::string read_text(const std::filesystem::path& path) {
  std::ifstream input(path, std::ios::binary);
  if (!input) throw std::runtime_error("cannot read " + path.string());
  return {std::istreambuf_iterator<char>(input), std::istreambuf_iterator<char>()};
}

std::string read_bytes(const std::filesystem::path& path) { return read_text(path); }

void write_text(const std::filesystem::path& path, const std::string& text) {
  std::ofstream output(path, std::ios::binary | std::ios::trunc);
  check(static_cast<bool>(output), "cannot write test fixture");
  output << text;
}

bool nearly_equal(double a, double b, double tolerance = 1e-12) {
  return std::abs(a - b) <= tolerance;
}

std::size_t count_buckets(const std::string& json, const std::string& section,
                          const std::string& channel) {
  const std::size_t section_pos = json.find("\"" + section + "\"");
  check(section_pos != std::string::npos, "missing " + section);
  const std::size_t array_pos = json.find("\"" + channel + "\": [", section_pos);
  check(array_pos != std::string::npos, "missing histogram channel " + channel);
  const std::size_t end = json.find(']', array_pos);
  check(end != std::string::npos, "unterminated histogram array");
  std::size_t count = 1;
  for (std::size_t i = array_pos; i < end; ++i) {
    if (json[i] == ',') ++count;
  }
  return count;
}

void test_parser() {
  const std::filesystem::path valid = "smoke-valid.cube";
  write_text(valid,
             "TITLE \"CRLF scientific\"\r\n"
             "LUT_3D_SIZE 2 # mixed comment\r\n"
             "DOMAIN_MIN 0e0 0e0 0e0\r\n"
             "DOMAIN_MAX 1e0 1e0 1e0\r\n"
             "0 0 0\r\n0 0 1\r\n0 1 0\r\n0 1 1\r\n"
             "1 0 0\r\n1 0 1\r\n1 1 0\r\n1 1 1\r\n");
  const CubeLut lut = parse_cube_file(valid);
  check(lut.size_3d == 2 && lut.table.size() == 8, "valid cube parse failed");

  const std::filesystem::path combined = "smoke-combined.cube";
  write_text(combined, "LUT_1D_SIZE 2\nLUT_3D_SIZE 2\nDOMAIN_MIN 0 0 0\nDOMAIN_MAX 1 1 1\n"
                       "0 0 0\n1 1 1\n"
                       "0 0 0\n0 0 1\n0 1 0\n0 1 1\n1 0 0\n1 0 1\n1 1 0\n1 1 1\n");
  const CubeLut combined_lut = parse_cube_file(combined);
  check(combined_lut.shaper_size == 2 && combined_lut.shaper.size() == 2 &&
            combined_lut.table.size() == 8,
        "combined LUT declared before entries was rejected");

  const std::filesystem::path invalid = "smoke-invalid.cube";
  write_text(invalid, "LUT_3D_SIZE 2\n0 0 0\n0 0 1\n0 1 0\n0 1 1\n"
                      "1 0 0\n1 0 1\n1 1 nan\n1 1 1\n");
  bool threw = false;
  try {
    (void)parse_cube_file(invalid);
  } catch (const CubeError&) {
    threw = true;
  }
  check(threw, "NaN LUT entry was accepted");

  const std::filesystem::path short_cube = "smoke-short.cube";
  write_text(short_cube, "LUT_3D_SIZE 2\n0 0 0\n0 0 1\n0 1 0\n");
  threw = false;
  try {
    (void)parse_cube_file(short_cube);
  } catch (const CubeError&) {
    threw = true;
  }
  check(threw, "short 3D LUT was accepted");

  const std::filesystem::path bad_domain = "smoke-domain.cube";
  write_text(bad_domain, "LUT_3D_SIZE 2\nDOMAIN_MIN 0 0 0\nDOMAIN_MAX 1 0 1\n"
                         "0 0 0\n0 0 1\n0 1 0\n0 1 1\n"
                         "1 0 0\n1 0 1\n1 1 0\n1 1 1\n");
  threw = false;
  try {
    (void)parse_cube_file(bad_domain);
  } catch (const CubeError&) {
    threw = true;
  }
  check(threw, "empty-domain axis was accepted");

  const std::filesystem::path shaper_only = "smoke-shaper.cube";
  write_text(shaper_only, "LUT_1D_SIZE 2\n0.0 0.0 0.0\n1.0 1.0 1.0\n");
  const CubeLut shaper_lut = parse_cube_file(shaper_only);
  check(shaper_lut.shaper_size == 2 && !shaper_lut.has_3d(), "1D-only LUT parse failed");
}

void test_kernel_and_color() {
  std::vector<Vec3> identity;
  for (int b = 0; b < 2; ++b) {
    for (int g = 0; g < 2; ++g) {
      for (int r = 0; r < 2; ++r) identity.push_back({double(r), double(g), double(b)});
    }
  }
  const auto at = [&identity](int r, int g, int b) {
    return identity[cube_index(2, r, g, b)];
  };
  for (const Vec3 p : {Vec3{0, 0, 0}, Vec3{0.25, 0.75, 0.4}, Vec3{1, 1, 1}}) {
    const Vec3 t = sample_trilinear(at, 2, p);
    const Vec3 q = sample_tetrahedral(at, 2, p);
    check(nearly_equal(t.r, p.r) && nearly_equal(t.g, p.g) && nearly_equal(t.b, p.b),
          "trilinear identity failed");
    check(nearly_equal(q.r, p.r) && nearly_equal(q.g, p.g) && nearly_equal(q.b, p.b),
          "tetrahedral identity failed");
  }

  CubeLut domain_lut;
  domain_lut.size_3d = 2;
  domain_lut.table = identity;
  domain_lut.domain_min = {0.1, 0.1, 0.1};
  domain_lut.domain_max = {0.9, 0.9, 0.9};
  const Vec3 clamped = apply_lut(domain_lut, {0.0, 0.5, 1.0}, Interpolation::Tetrahedral);
  check(nearly_equal(clamped.r, 0.0) && nearly_equal(clamped.g, 0.5) && nearly_equal(clamped.b, 1.0),
        "domain normalization or boundary clamp failed");

  const Mat3 matrix = rgb_to_xyz_matrix(ColorSpaceId::Rec709);
  const Mat3 product = multiply(inverse(matrix), matrix);
  for (int row = 0; row < 3; ++row) {
    for (int col = 0; col < 3; ++col) {
      check(nearly_equal(product.m[row][col], row == col ? 1.0 : 0.0, 1e-10),
            "matrix inverse failed");
    }
  }
  const Vec3 white = convert_linear_rgb(ColorSpaceId::AcesCg, ColorSpaceId::Rec709, {1, 1, 1});
  check(nearly_equal(white.r, 1.0, 1e-10) && nearly_equal(white.g, 1.0, 1e-10) &&
            nearly_equal(white.b, 1.0, 1e-10),
        "Bradford white adaptation failed");
  const Vec3 srgb = convert_encoded_rgb(ColorSpaceId::Srgb, ColorSpaceId::Srgb, {0.2, 0.5, 0.8});
  check(nearly_equal(srgb.r, 0.2, 1e-12) && nearly_equal(srgb.g, 0.5, 1e-12) &&
            nearly_equal(srgb.b, 0.8, 1e-12),
        "identity transfer conversion failed");

  for (const ColorSpaceId space :
       {ColorSpaceId::Srgb, ColorSpaceId::Rec709, ColorSpaceId::Rec2020,
        ColorSpaceId::DciP3, ColorSpaceId::AcesCg}) {
    const Mat3 rgb_to_xyz = rgb_to_xyz_matrix(space);
    const Mat3 identity = multiply(inverse(rgb_to_xyz), rgb_to_xyz);
    for (int row = 0; row < 3; ++row) {
      for (int col = 0; col < 3; ++col) {
        check(nearly_equal(identity.m[row][col], row == col ? 1.0 : 0.0, 1e-10),
              std::string("matrix inverse failed for ") + color_space_name(space));
      }
    }
    const double linear = 0.37;
    check(nearly_equal(decode_transfer(space, encode_transfer(space, linear)), linear, 1e-10),
          std::string("transfer round-trip failed for ") + color_space_name(space));
  }
}

void test_image_io() {
  FloatImage gray;
  gray.width = 3;
  gray.height = 2;
  gray.channels = 1;
  gray.pixels = {0.0F, 0.5F, 1.0F, 0.125F, 0.25F, 0.75F};
  const std::filesystem::path pgm = "smoke-gray.pgm";
  write_image_atomic(pgm, gray);
  const FloatImage restored = read_image(pgm);
  check(restored.width == 3 && restored.height == 2 && restored.channels == 1,
        "PGM dimensions or channel count changed");
  check(read_text(pgm).rfind("P5\n3 2\n65535\n", 0) == 0, "PGM was not written as 16-bit P5");
  for (std::size_t i = 0; i < gray.pixels.size(); ++i) {
    check(std::abs(restored.pixels[i] - gray.pixels[i]) <= 1.0F / 65535.0F,
          "PGM round-trip changed a sample");
  }

  FloatImage mismatched;
  mismatched.width = 2;
  mismatched.height = 2;
  mismatched.channels = 3;
  mismatched.pixels = std::vector<float>(3, 0.5F);
  bool threw = false;
  try {
    write_image_atomic("smoke-mismatch.ppm", mismatched);
  } catch (const ImageIoError&) {
    threw = true;
  }
  check(threw, "writer accepted mismatched dimensions");

  const std::filesystem::path eight_bit = "smoke-8bit.ppm";
  {
    std::ofstream output(eight_bit, std::ios::binary | std::ios::trunc);
    check(static_cast<bool>(output), "cannot create 8-bit PPM fixture");
    output << "P6\n2 1\n255\n";
    const unsigned char samples[] = {0, 51, 102, 153, 204, 255};
    output.write(reinterpret_cast<const char*>(samples), sizeof(samples));
  }
  const FloatImage eight_restored = read_image(eight_bit);
  check(eight_restored.width == 2 && eight_restored.height == 1 &&
            eight_restored.channels == 3,
        "8-bit PPM dimensions changed");
  check(nearly_equal(eight_restored.pixels[0], 0.0) &&
            nearly_equal(eight_restored.pixels[1], 51.0 / 255.0, 1e-6) &&
            nearly_equal(eight_restored.pixels[5], 1.0, 1e-6),
        "8-bit PPM samples were decoded with the wrong width");

  const std::filesystem::path transaction_image = "smoke-transaction.ppm";
  const std::filesystem::path transaction_report = "smoke-report-dir";
  write_text(transaction_image, "previous image");
  std::error_code error;
  std::filesystem::create_directory(transaction_report, error);
  check(!error, "cannot create transaction test directory");
  threw = false;
  try {
    FloatImage small;
    small.width = 1;
    small.height = 1;
    small.channels = 3;
    small.pixels = {0.1F, 0.2F, 0.3F};
    write_image_and_text_atomic(transaction_image, small, transaction_report, "report");
  } catch (const ImageIoError&) {
    threw = true;
  }
  check(threw, "two-file transaction accepted a directory report path");
  check(read_text(transaction_image) == "previous image",
        "two-file transaction did not restore the previous image");
  bool leftover = false;
  for (const auto& entry : std::filesystem::directory_iterator(".")) {
    if (entry.path().filename().string().find(".tmp.") != std::string::npos) {
      leftover = true;
    }
  }
  check(!leftover, "two-file transaction left a temporary file behind");
}

void test_end_to_end(const std::string& executable, const std::filesystem::path& work) {
  const std::filesystem::path frames_a = work / "frames-a";
  const std::filesystem::path frames_b = work / "frames-b";
  const std::filesystem::path out1 = work / "graded-1.ppm";
  const std::filesystem::path out4 = work / "graded-4.ppm";
  const std::filesystem::path report = work / "report.json";

  check(run_cli(executable, "gen-fixture --seed 7 --size 96x72 --out " + quote(frames_a.string())) == 0,
        "gen-fixture failed");
  check(run_cli(executable, "gen-fixture --seed 7 --size 96x72 --out " + quote(frames_b.string())) == 0,
        "second gen-fixture failed");
  check(read_bytes(frames_a / "log_ramp.ppm") == read_bytes(frames_b / "log_ramp.ppm"),
        "fixture image is not deterministic");
  check(read_bytes(frames_a / "color_bars.ppm") == read_bytes(frames_b / "color_bars.ppm"),
        "fixture color bars are not deterministic");
  check(read_bytes(frames_a / "grade33.cube") == read_bytes(frames_b / "grade33.cube"),
        "fixture LUT is not deterministic");

  const std::string lut = quote((frames_a / "grade33.cube").string());
  const std::string image = quote((frames_a / "log_ramp.ppm").string());
  const std::string common = " --src acescg --dst rec709 --interp tetra --threads ";
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "1 --out " +
                               quote(out1.string()) + " --report " + quote(report.string())) == 0,
        "single-thread grade failed");
  const std::string first_image = read_bytes(out1);
  const std::string first_report = read_text(report);
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "1 --out " +
                               quote(out1.string()) + " --report " + quote(report.string())) == 0,
        "deterministic overwrite grade failed");
  check(read_bytes(out1) == first_image && read_text(report) == first_report,
        "repeated grade did not reproduce identical bytes");
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "4 --out " +
                               quote(out4.string()) + " --report " + quote((work / "report4.json").string())) == 0,
        "four-thread grade failed");
  check(read_bytes(out1) == read_bytes(out4), "threads 1 and 4 produced different images");

  const std::string json = read_text(report);
  check(json.find("\"hist_bins\": 256") != std::string::npos, "report hist_bins missing");
  for (const char* section : {"hist_before", "hist_after"}) {
    for (const char* channel : {"r", "g", "b"}) {
      check(count_buckets(json, section, channel) == 256,
            std::string("report bucket count mismatch in ") + section + "." + channel);
    }
  }

  const std::filesystem::path verify_output = work / "verify.txt";
  check(run_cli(executable, "verify --lut " + lut + " --in " + image, verify_output) == 0,
        "verify failed");
  const std::string summary = read_text(verify_output);
  check(summary.find("scalar_vs_tiled_max_dev=0.000000") != std::string::npos,
        "verify summary missing zero scalar/tiled deviation");

  check(run_cli(executable, "grade", work / "missing-args.txt") == 1, "parameter error code mismatch");
  check(run_cli(executable, "verify --lut " + quote((work / "missing.cube").string()) + " --in " + image,
                work / "missing-lut.txt") == 2, "LUT error code mismatch");

  const std::filesystem::path bad_count = work / "bad-count.cube";
  write_text(bad_count, "LUT_3D_SIZE 2\n0 0 0\n0 0 1\n0 1 0\n");
  check(run_cli(executable, "verify --lut " + quote(bad_count.string()) + " --in " + image,
                work / "bad-count.txt") == 2, "short LUT error code mismatch");

  const std::filesystem::path zero_output = work / "zero.ppm";
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "0 --out " +
                               quote(zero_output.string()) + " --report " +
                               quote((work / "zero.json").string()),
                work / "zero-threads.txt") == 1, "zero-thread error code mismatch");
  check(!std::filesystem::exists(zero_output), "zero-thread run left an image");

  const std::filesystem::path collision = work / "collision.ppm";
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "4 --out " +
                               quote(collision.string()) + " --report " + quote(collision.string()),
                work / "collision.txt") == 1, "shared output paths were not rejected");
  check(!std::filesystem::exists(collision), "shared output paths left a file");

  const std::filesystem::path nested_output = work / "nested" / "frames" / "graded.ppm";
  const std::filesystem::path nested_report = work / "nested" / "reports" / "report.json";
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "4 --out " +
                               quote(nested_output.string()) + " --report " +
                               quote(nested_report.string())) == 0,
        "grade did not create missing output directories");
  check(std::filesystem::exists(nested_output) && std::filesystem::exists(nested_report),
        "grade did not commit both outputs");

  const std::filesystem::path fixture_rollback = work / "fixture-rollback";
  check(std::filesystem::create_directories(fixture_rollback),
        "cannot create fixture rollback directory");
  write_text(fixture_rollback / "log_ramp.ppm", "old ramp");
  write_text(fixture_rollback / "grade33.cube", "old cube");
  check(std::filesystem::create_directory(fixture_rollback / "color_bars.ppm"),
        "cannot create colliding fixture path");
  check(run_cli(executable, "gen-fixture --seed 7 --size 8x8 --out " +
                               quote(fixture_rollback.string()),
                work / "fixture-rollback.txt") == 3,
        "gen-fixture collision error code mismatch");
  check(read_text(fixture_rollback / "log_ramp.ppm") == "old ramp" &&
            read_text(fixture_rollback / "grade33.cube") == "old cube",
        "gen-fixture did not restore previous outputs");
  bool fixture_leftover = false;
  for (const auto& entry : std::filesystem::directory_iterator(fixture_rollback)) {
    if (entry.path().filename().string().find(".tmp.") != std::string::npos ||
        entry.path().filename().string().find(".bak.") != std::string::npos) {
      fixture_leftover = true;
    }
  }
  check(!fixture_leftover, "gen-fixture left staging or backup files behind");

  const std::filesystem::path report_is_directory = work / "report-is-directory";
  const std::filesystem::path blocked_output = work / "blocked-output.ppm";
  check(std::filesystem::create_directory(report_is_directory),
        "cannot create report directory collision");
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "4 --out " +
                               quote(blocked_output.string()) + " --report " +
                               quote(report_is_directory.string()),
                work / "report-directory.txt") == 3,
        "directory report error code mismatch");
  check(!std::filesystem::exists(blocked_output),
        "report failure left the graded image behind");

  const std::filesystem::path truncated = work / "truncated.ppm";
  const std::string ppm = read_bytes(frames_a / "log_ramp.ppm");
  write_text(truncated, ppm.substr(0, ppm.size() - 3));
  check(run_cli(executable, "verify --lut " + lut + " --in " + quote(truncated.string()),
                work / "truncated.txt") == 3, "truncated image error code mismatch");

  const std::filesystem::path forbidden_report = work / "forbidden.json";
  check(run_cli(executable, "grade --lut " + lut + " --in " + image + common + "4 --out " +
                               quote(frames_a.string()) + " --report " + quote(forbidden_report.string()),
                work / "unwritable.txt") == 3, "unwritable output error code mismatch");
  check(!std::filesystem::exists(forbidden_report), "partial report left after output failure");
}

}  // namespace

int main(int argc, char** argv) {
  try {
    check(argc == 3, "usage: lutgrade_smoke <lutgrade> <work-directory>");
    const std::filesystem::path work = argv[2];
    std::error_code error;
    std::filesystem::remove_all(work, error);
    std::filesystem::create_directories(work, error);
    check(!error, "cannot create smoke work directory");
    if (!std::filesystem::current_path().filename().empty()) {
      // Keep generated parser fixtures inside the build work directory.
      std::filesystem::current_path(work, error);
      check(!error, "cannot enter smoke work directory");
    }
    test_parser();
    test_kernel_and_color();
    test_image_io();
    test_end_to_end(argv[1], work);
    std::cout << "smoke: ok\n";
    return 0;
  } catch (const std::exception& error) {
    std::cerr << "smoke: " << error.what() << '\n';
    return 1;
  }
}

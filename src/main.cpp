#include <algorithm>
#include <cctype>
#include <cstdint>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <map>
#include <new>
#include <stdexcept>
#include <string>
#include <vector>

#include "color_matrix.h"
#include "cube_parser.h"
#include "histogram.h"
#include "image_io.h"
#include "render.h"

namespace lutgrade {
void generate_fixture(std::uint32_t seed, std::uint32_t width, std::uint32_t height,
                      const std::filesystem::path& output_directory);
}

namespace {

using lutgrade::ColorError;
using lutgrade::ColorSpaceId;
using lutgrade::CubeError;
using lutgrade::CubeLut;
using lutgrade::FloatImage;
using lutgrade::GradeReport;
using lutgrade::ImageIoError;
using lutgrade::Interpolation;
using lutgrade::RenderOptions;

constexpr double kVerificationThreshold = 1e-6;

class CliError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

void print_usage(std::ostream& stream) {
  stream << "Usage:\n"
         << "  lutgrade gen-fixture --seed N --size WxH --out DIR\n"
         << "  lutgrade grade --lut FILE --in IMAGE --src SPACE --dst SPACE --interp tri|tetra "
            "--threads N --out IMAGE --report JSON\n"
         << "  lutgrade verify --lut FILE --in IMAGE [--src SPACE] [--dst SPACE] "
            "[--interp tri|tetra] [--threads N]\n"
         << "Color spaces: srgb, rec709, rec2020, dci-p3, acescg\n";
}

std::uint32_t parse_unsigned(const std::string& text, const std::string& label,
                             std::uint32_t maximum) {
  if (text.empty() ||
      !std::all_of(text.begin(), text.end(), [](unsigned char ch) { return std::isdigit(ch); })) {
    throw CliError("invalid " + label + ": " + text);
  }
  std::uint64_t value = 0;
  for (const char ch : text) {
    const std::uint64_t digit = static_cast<std::uint64_t>(ch - '0');
    if (value > (static_cast<std::uint64_t>(maximum) - digit) / 10ULL) {
      throw CliError(label + " is out of range: " + text);
    }
    value = value * 10ULL + digit;
  }
  return static_cast<std::uint32_t>(value);
}

std::pair<std::uint32_t, std::uint32_t> parse_size(const std::string& text) {
  const std::size_t separator = text.find_first_of("xX");
  if (separator == std::string::npos || text.find_first_of("xX", separator + 1) != std::string::npos) {
    throw CliError("size must have the form WIDTHxHEIGHT: " + text);
  }
  const std::uint32_t width = parse_unsigned(text.substr(0, separator), "width", 65535);
  const std::uint32_t height = parse_unsigned(text.substr(separator + 1), "height", 65535);
  if (width == 0 || height == 0) {
    throw CliError("width and height must be positive");
  }
  return {width, height};
}

std::map<std::string, std::string> parse_options(int argc, char** argv, int first,
                                                 std::initializer_list<const char*> allowed) {
  std::map<std::string, std::string> options;
  for (int i = first; i < argc; ++i) {
    const std::string option = argv[i];
    if (option.rfind("--", 0) != 0 || std::find_if(allowed.begin(), allowed.end(), [&](const char* item) {
          return option == item;
        }) == allowed.end()) {
      throw CliError("unknown option: " + option);
    }
    if (options.count(option) != 0) {
      throw CliError("duplicate option: " + option);
    }
    if (i + 1 >= argc || std::string(argv[i + 1]).rfind("--", 0) == 0) {
      throw CliError("missing value for " + option);
    }
    options[option] = argv[++i];
  }
  return options;
}

void require(const std::map<std::string, std::string>& options,
             std::initializer_list<const char*> names) {
  for (const char* name : names) {
    if (options.count(name) == 0) {
      throw CliError(std::string("missing required option: ") + name);
    }
  }
}

Interpolation parse_interpolation(const std::string& text) {
  if (text == "tri" || text == "trilinear") return Interpolation::Trilinear;
  if (text == "tetra" || text == "tetrahedral") return Interpolation::Tetrahedral;
  throw CliError("invalid interpolation: " + text);
}

const char* interpolation_name(Interpolation interpolation) {
  return interpolation == Interpolation::Tetrahedral ? "tetrahedral" : "trilinear";
}

RenderOptions make_render_options(ColorSpaceId source, ColorSpaceId destination,
                                  Interpolation interpolation) {
  RenderOptions result;
  result.source = source;
  result.destination = destination;
  result.interpolation = interpolation;
  result.transform_color = true;
  return result;
}

bool same_output_path(const std::filesystem::path& first,
                      const std::filesystem::path& second) {
  std::error_code error;
  const auto first_path = std::filesystem::absolute(first, error).lexically_normal();
  if (error) return false;
  const auto second_path = std::filesystem::absolute(second, error).lexically_normal();
  return !error && first_path == second_path;
}

void print_summary(const GradeReport& report) {
  std::cout << std::fixed << std::setprecision(6)
            << "lut_grid_3d=" << report.grid_3d << "x" << report.grid_3d << "x" << report.grid_3d
            << " shaper_size=" << report.shaper_size
            << " max_dev_r=" << report.deviation.red
            << " max_dev_g=" << report.deviation.green
            << " max_dev_b=" << report.deviation.blue
            << " hist_bins=256"
            << " hist_before_nonzero=" << report.before.nonzero_count()
            << " hist_after_nonzero=" << report.after.nonzero_count()
            << " scalar_vs_tiled_max_dev=" << report.deviation.maximum() << '\n';
}

GradeReport evaluate(const CubeLut& lut, const FloatImage& image, const RenderOptions& options,
                     unsigned int threads, FloatImage* rendered = nullptr) {
  const FloatImage scalar = lutgrade::render_scalar(lut, image, options);
  const FloatImage tiled = lutgrade::render_tiled(lut, image, options, threads);
  if (rendered != nullptr) {
    *rendered = tiled;
  }
  GradeReport report;
  report.grid_3d = lut.size_3d;
  report.shaper_size = lut.shaper_size;
  report.width = image.width;
  report.height = image.height;
  report.threads = threads;
  report.source = lutgrade::color_space_name(options.source);
  report.destination = lutgrade::color_space_name(options.destination);
  report.interpolation = interpolation_name(options.interpolation);
  report.before = lutgrade::compute_histogram(image);
  report.after = lutgrade::compute_histogram(tiled);
  report.deviation = lutgrade::max_abs_deviation(scalar, tiled);
  report.threshold = kVerificationThreshold;
  report.verification = report.deviation.maximum() <= kVerificationThreshold;
  return report;
}

int run_gen_fixture(int argc, char** argv) {
  const auto options = parse_options(argc, argv, 2, {"--seed", "--size", "--out"});
  require(options, {"--seed", "--size", "--out"});
  const std::uint32_t seed = parse_unsigned(options.at("--seed"), "seed", 0xffffffffU);
  const auto [width, height] = parse_size(options.at("--size"));
  const std::filesystem::path output = options.at("--out");
  lutgrade::generate_fixture(seed, width, height, output);
  std::cout << "generated seed=" << seed << " size=" << width << 'x' << height
            << " out=" << output.string() << '\n';
  return 0;
}

int run_grade(int argc, char** argv) {
  const auto options = parse_options(
      argc, argv, 2, {"--lut", "--in", "--src", "--dst", "--interp", "--threads", "--out", "--report"});
  require(options, {"--lut", "--in", "--src", "--dst", "--interp", "--threads", "--out", "--report"});
  const ColorSpaceId source = lutgrade::parse_color_space(options.at("--src"));
  const ColorSpaceId destination = lutgrade::parse_color_space(options.at("--dst"));
  const Interpolation interpolation = parse_interpolation(options.at("--interp"));
  const std::uint32_t parsed_threads = parse_unsigned(options.at("--threads"), "threads", 1024);
  if (parsed_threads == 0) throw CliError("threads must be positive");
  const unsigned int threads = static_cast<unsigned int>(parsed_threads);
  const std::filesystem::path output = options.at("--out");
  const std::filesystem::path report_path = options.at("--report");
  if (same_output_path(output, report_path)) {
    throw CliError("--out and --report must be different paths");
  }
  const CubeLut lut = lutgrade::parse_cube_file(options.at("--lut"));
  const FloatImage image = lutgrade::as_rgb(lutgrade::read_image(options.at("--in")));
  lutgrade::check_output_writable(output);
  lutgrade::check_output_writable(report_path);
  const RenderOptions render_options = make_render_options(source, destination, interpolation);
  FloatImage graded;
  const GradeReport report = evaluate(lut, image, render_options, threads, &graded);

  lutgrade::write_image_and_text_atomic(
      output, graded, report_path,
      lutgrade::report_json(report, options.at("--lut"), options.at("--in"), output.string()));
  print_summary(report);
  return report.verification ? 0 : 4;
}

int run_verify(int argc, char** argv) {
  const auto options = parse_options(
      argc, argv, 2, {"--lut", "--in", "--src", "--dst", "--interp", "--threads"});
  require(options, {"--lut", "--in"});
  const std::string source_name = options.count("--src") ? options.at("--src") : "acescg";
  const std::string destination_name = options.count("--dst") ? options.at("--dst") : "rec709";
  const ColorSpaceId source = lutgrade::parse_color_space(source_name);
  const ColorSpaceId destination = lutgrade::parse_color_space(destination_name);
  const Interpolation interpolation =
      parse_interpolation(options.count("--interp") ? options.at("--interp") : "tetra");
  const std::uint32_t parsed_threads =
      parse_unsigned(options.count("--threads") ? options.at("--threads") : "4", "threads", 1024);
  if (parsed_threads == 0) throw CliError("threads must be positive");
  const CubeLut lut = lutgrade::parse_cube_file(options.at("--lut"));
  const FloatImage image = lutgrade::as_rgb(lutgrade::read_image(options.at("--in")));
  const RenderOptions render_options = make_render_options(source, destination, interpolation);
  const GradeReport report =
      evaluate(lut, image, render_options, static_cast<unsigned int>(parsed_threads));
  print_summary(report);
  if (!report.verification) {
    std::cerr << "error: scalar/tiled deviation exceeds 1e-6\n";
    return 4;
  }
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) {
    print_usage(std::cerr);
    return 1;
  }
  for (int i = 1; i < argc; ++i) {
    if (std::string(argv[i]) == "--help" || std::string(argv[i]) == "-h") {
      print_usage(std::cout);
      return 0;
    }
  }

  try {
    const std::string command = argv[1];
    if (command == "gen-fixture") return run_gen_fixture(argc, argv);
    if (command == "grade") return run_grade(argc, argv);
    if (command == "verify") return run_verify(argc, argv);
    throw CliError("unknown command: " + command);
  } catch (const CliError& error) {
    std::cerr << "error: " << error.what() << '\n';
    return 1;
  } catch (const ColorError& error) {
    std::cerr << "error: " << error.what() << '\n';
    return 1;
  } catch (const std::invalid_argument& error) {
    std::cerr << "error: " << error.what() << '\n';
    return 1;
  } catch (const CubeError& error) {
    std::cerr << "error: LUT: " << error.what() << '\n';
    return 2;
  } catch (const ImageIoError& error) {
    std::cerr << "error: " << error.what() << '\n';
    return 3;
  } catch (const std::bad_alloc&) {
    std::cerr << "error: insufficient memory\n";
    return 3;
  } catch (const std::exception& error) {
    std::cerr << "error: " << error.what() << '\n';
    return 3;
  }
}

#include <iostream>
#include <string>
#include <string_view>
#include <vector>

#include "error.hpp"

namespace {

using media_restore::Diagnostic;
using media_restore::ErrorCode;

void usage(std::ostream& out) {
  out << "usage: media-restore <command> [options]\n"
         "\n"
         "commands:\n"
         "  generate --out DIR [--include-damaged]\n"
         "      Create a deterministic sample archive and fixtures.\n"
         "  inspect --manifest FILE [--json FILE]\n"
         "      Validate and describe an archive manifest.\n"
         "  restore --manifest FILE --rate RATE --layout LAYOUT --loudness LUFS\n"
         "          [--out DIR] [--json FILE]\n"
         "      Validate media, order tracks, restore audio and hash outputs.\n"
         "  verify --manifest FILE --result FILE\n"
         "      Re-check a restore result against the archive.\n"
         "\n"
         "exit codes: 0 verified, 1 usage/input error, 2 media validation\n"
         "            failure, 3 restore or dependency failure\n";
}

struct Args {
  std::string command;
  std::vector<std::pair<std::string, std::string>> options;
  std::vector<std::string> flags;

  bool has(const std::string& name) const {
    for (const auto& flag : flags) {
      if (flag == name) return true;
    }
    return false;
  }

  bool get(const std::string& name, std::string* value) const {
    for (const auto& option : options) {
      if (option.first == name) {
        *value = option.second;
        return true;
      }
    }
    return false;
  }
};

bool parse_args(int argc, char** argv, Args* args, Diagnostic* error) {
  if (argc < 2) {
    error->code = ErrorCode::usage;
    error->message = "missing command";
    return false;
  }
  args->command = argv[1];
  for (int i = 2; i < argc; ++i) {
    std::string_view arg = argv[i];
    if (arg == "--help" || arg == "-h") {
      args->flags.emplace_back("--help");
      continue;
    }
    if (arg.rfind("--", 0) != 0) {
      error->code = ErrorCode::usage;
      error->message = "unexpected positional argument: " + std::string(arg);
      return false;
    }
    std::string name(arg);
    const auto eq = name.find('=');
    if (eq != std::string::npos) {
      args->options.emplace_back(name.substr(0, eq), name.substr(eq + 1));
      continue;
    }
    if (i + 1 >= argc) {
      error->code = ErrorCode::usage;
      error->message = "option requires a value: " + name;
      return false;
    }
    args->options.emplace_back(std::move(name), argv[++i]);
  }
  return true;
}

}  // namespace

int main(int argc, char** argv) {
  Args args;
  Diagnostic error;
  if (!parse_args(argc, argv, &args, &error)) {
    media_restore::print_diagnostics({error});
    usage(std::cerr);
    return media_restore::exit_code_for(error.code);
  }

  if (args.command == "--help" || args.command == "help" || args.has("--help")) {
    usage(std::cout);
    return 0;
  }

  if (args.command == "generate" || args.command == "inspect" ||
      args.command == "restore" || args.command == "verify") {
    std::cerr << "error code=E_INTERNAL message=command not implemented yet: "
              << args.command << '\n';
    return 3;
  }

  std::cerr << "error code=E_USAGE message=unknown command: " << args.command
            << '\n';
  usage(std::cerr);
  return 1;
}

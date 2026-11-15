#include "subtitle_audit/command_utils.hpp"

#include <iostream>

namespace audit {

CliOptions parse_cli_options(int argc, char **argv, int begin) {
    CliOptions options;
    for (int i = begin; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg.rfind("--", 0) != 0) {
            options.positional.push_back(arg);
            continue;
        }
        const std::string key = arg.substr(2);
        if (key.empty()) {
            options.error = "empty option";
            return options;
        }
        if (i + 1 < argc && std::string(argv[i + 1]).rfind("--", 0) != 0) {
            options.values[key] = argv[++i];
        } else {
            options.values[key] = "1";
        }
    }
    return options;
}

bool check_cli_options(const CliOptions &options,
                       const std::vector<std::string> &allowed,
                       const std::vector<std::string> &required, std::string &error) {
    if (!options.positional.empty()) {
        error = "unexpected argument: " + options.positional.front();
        return false;
    }
    for (const auto &[key, value] : options.values) {
        (void)value;
        bool ok = false;
        for (const std::string &name : allowed) ok = ok || name == key;
        if (!ok) { error = "unknown option --" + key; return false; }
    }
    for (const std::string &key : required) {
        if (!options.values.count(key)) { error = "missing --" + key; return false; }
    }
    return true;
}

int parse_int_option(const CliOptions &options, const std::string &name, int fallback,
                     int minimum, int maximum, std::string &error) {
    if (!options.values.count(name)) return fallback;
    try {
        const int value = std::stoi(options.values.at(name));
        if (value < minimum || value > maximum) throw std::out_of_range(name);
        return value;
    } catch (...) {
        error = "invalid --" + name;
        return fallback;
    }
}

void print_diagnostics(const AssDocument &doc) {
    for (const auto &diag : doc.diagnostics)
        std::cerr << "diagnostic: line " << diag.line << ": " << diag.message << '\n';
}

}  // namespace audit

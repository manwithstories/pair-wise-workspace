#pragma once

#include <map>
#include <string>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"

namespace audit {

struct CliOptions {
    std::map<std::string, std::string> values;
    std::vector<std::string> positional;
    std::string error;
};

CliOptions parse_cli_options(int argc, char **argv, int begin);
bool check_cli_options(const CliOptions &options,
                       const std::vector<std::string> &allowed,
                       const std::vector<std::string> &required, std::string &error);
int parse_int_option(const CliOptions &options, const std::string &name, int fallback,
                     int minimum, int maximum, std::string &error);
void print_diagnostics(const AssDocument &doc);
int run_batch_command(int argc, char **argv);

}  // namespace audit

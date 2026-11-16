#pragma once
#include "cli.hpp"

// Subcommand entry points. main() parses with CLI11, validates via cli.hpp,
// then calls one of these. Each returns the process exit code and throws Error
// for failures; main() maps Error to its code and prints the message.
namespace lutgrade {

int execute_fixture(const FixtureOptions& options);
int execute_render(const RenderOptions& options);
int execute_compare(const CompareOptions& options);

}  // namespace lutgrade

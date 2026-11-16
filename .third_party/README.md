# Third-party dependencies (vendored)

The build must work offline, so the two pinned dependencies are checked in
verbatim rather than fetched by CMake.

| Library | Version | Upstream | License | Files |
|---|---|---|---|---|
| CLI11 | 2.4.2 | https://github.com/CLIUtils/CLI11 | BSD-3-Clause (`CLI/LICENSE`) | `CLI/CLI/**` |
| Catch2 | 3.5.2 | https://github.com/catchorg/Catch2 | BSL-1.0 (`Catch2/LICENSE.txt`) | `Catch2/catch_amalgamated.{hpp,cpp}` |

Catch2 is used only by `tests/integration.cpp`; CLI11 is used only by the
`lutgrade` executable. Neither is modified.

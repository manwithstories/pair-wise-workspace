#!/bin/sh
set -eu

cmake -S "$(dirname "$0")" -B build -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build --parallel
ctest --test-dir build --output-on-failure

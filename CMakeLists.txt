cmake_minimum_required(VERSION 3.20)
project(h264_audit LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

set(H264_CORE_SOURCES
    src/bit_reader.cpp
    src/nal_split.cpp
    src/param_sets.cpp
    src/slice_header.cpp
    src/picture_order.cpp
    src/access_unit.cpp
    src/gop_index.cpp
    src/stats.cpp
    src/report.cpp
)

add_executable(h264_audit
    ${H264_CORE_SOURCES}
    src/main.cpp
    tests/selftest.cpp
)

target_include_directories(h264_audit PRIVATE src tests)
target_compile_options(h264_audit PRIVATE -Wall -Wextra)

add_executable(fixture_gen tools/make_fixtures.cpp)
target_compile_options(fixture_gen PRIVATE -Wall -Wextra)

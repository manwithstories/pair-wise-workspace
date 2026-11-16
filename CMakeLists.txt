cmake_minimum_required(VERSION 3.20)
project(h264inspect LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_library(h264lib STATIC
  src/bit_reader.cpp
  src/bit_writer.cpp
  src/exp_golomb.cpp
  src/nal_scanner.cpp
  src/sps_parser.cpp
  src/slice_parser.cpp
  src/gop_analyzer.cpp
  src/report_writer.cpp
  src/fixture_writer.cpp
)
target_include_directories(h264lib PUBLIC src)

enable_testing()

add_executable(test_bit_reader tests/test_bit_reader.cpp tests/test_framework.cpp)
target_link_libraries(test_bit_reader PRIVATE h264lib)
add_test(NAME bit_reader COMMAND test_bit_reader)

add_executable(test_exp_golomb tests/test_exp_golomb.cpp tests/test_framework.cpp)
target_link_libraries(test_exp_golomb PRIVATE h264lib)
add_test(NAME exp_golomb COMMAND test_exp_golomb)

add_executable(test_nal_scanner tests/test_nal_scanner.cpp tests/test_framework.cpp)
target_link_libraries(test_nal_scanner PRIVATE h264lib)
add_test(NAME nal_scanner COMMAND test_nal_scanner)

add_executable(test_parsers tests/test_parsers.cpp tests/test_framework.cpp)
target_link_libraries(test_parsers PRIVATE h264lib)
add_test(NAME parsers COMMAND test_parsers)

add_executable(test_slice tests/test_slice.cpp tests/test_framework.cpp)
target_link_libraries(test_slice PRIVATE h264lib)
add_test(NAME slice COMMAND test_slice)

add_executable(test_gop tests/test_gop.cpp tests/test_framework.cpp)
target_link_libraries(test_gop PRIVATE h264lib)
add_test(NAME gop COMMAND test_gop)

add_executable(test_report tests/test_report.cpp tests/test_framework.cpp)
target_link_libraries(test_report PRIVATE h264lib)
add_test(NAME report COMMAND test_report)

add_executable(generate_fixture tests/generate_fixture.cpp)
target_link_libraries(generate_fixture PRIVATE h264lib)
add_test(NAME generate_fixture
         COMMAND generate_fixture ${CMAKE_BINARY_DIR}/fixture_sample.264 24 3 0)

add_executable(h264inspect src/main.cpp)
target_link_libraries(h264inspect PRIVATE h264lib)

add_executable(integration tests/integration.cpp tests/test_framework.cpp)
target_link_libraries(integration PRIVATE h264lib)
add_test(NAME integration COMMAND integration
         ${CMAKE_BINARY_DIR}/fixture_sample.264)

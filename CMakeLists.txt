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

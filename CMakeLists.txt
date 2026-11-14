cmake_minimum_required(VERSION 3.24)
project(h264probe LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

add_compile_options(-Wall -Wextra)

add_library(h264core STATIC
  src/bitreader.cpp
  src/annexb.cpp
  src/nal.cpp
  src/sps.cpp
  src/pps.cpp
  src/slice.cpp
  src/sei.cpp
  src/picture.cpp
  src/report.cpp
  src/json.cpp
  src/diff.cpp
  src/analyzer.cpp
)
target_include_directories(h264core PUBLIC src)

add_executable(h264probe src/main.cpp)
target_link_libraries(h264probe PRIVATE h264core)

enable_testing()
add_executable(h264core_tests tests/test_main.cpp)
target_link_libraries(h264core_tests PRIVATE h264core)
add_test(NAME h264core_tests COMMAND h264core_tests)

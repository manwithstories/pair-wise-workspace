cmake_minimum_required(VERSION 3.28)

project(relay LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE RelWithDebInfo)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)
find_package(OpenSSL 3 REQUIRED)
find_package(Catch2 3 REQUIRED)

add_library(relay_core STATIC
  src/types.cpp
  src/hash.cpp
  src/fixture.cpp
  src/frame_reader.cpp
  src/packet_reorder.cpp
  src/timeline.cpp
)
target_include_directories(relay_core PUBLIC include)
target_link_libraries(relay_core PUBLIC PkgConfig::FFMPEG OpenSSL::Crypto)
target_compile_options(relay_core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(relay app/main.cpp)
target_link_libraries(relay PRIVATE relay_core)

enable_testing()
add_executable(relay_tests tests/pipeline_test.cpp)
target_link_libraries(relay_tests PRIVATE relay_core Catch2::Catch2WithMain)
include(Catch)
catch_discover_tests(relay_tests)

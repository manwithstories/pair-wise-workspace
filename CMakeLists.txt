cmake_minimum_required(VERSION 3.30)

project(mediaqc VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE RelWithDebInfo CACHE STRING "Build type" FORCE)
endif()

# Homebrew installs into /opt/homebrew on Apple Silicon and /usr/local on
# Intel; adding both keeps the build portable across the two macOS targets.
list(APPEND CMAKE_PREFIX_PATH /opt/homebrew /usr/local)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat
  libavcodec
  libswresample
  libavutil
)

find_package(Catch2 3 CONFIG REQUIRED)
find_package(Threads REQUIRED)

add_library(mediaqc_core STATIC
  src/checksum.cpp
  src/json.cpp
  src/timeline.cpp
  src/types.cpp
)

target_include_directories(mediaqc_core PUBLIC include)
target_link_libraries(mediaqc_core PUBLIC PkgConfig::FFMPEG Threads::Threads)
target_compile_options(mediaqc_core PRIVATE -Wall -Wextra -Wpedantic -Wshadow)

add_executable(mediaqc app/main.cpp)
target_link_libraries(mediaqc PRIVATE mediaqc_core)

include(CTest)
if(BUILD_TESTING)
  include(Catch)
  add_executable(mediaqc_tests
    tests/pipeline_tests.cpp
    tests/timeline_tests.cpp
    tests/fixture_factory.cpp
  )
  target_link_libraries(mediaqc_tests PRIVATE mediaqc_core Catch2::Catch2WithMain)
  target_include_directories(mediaqc_tests PRIVATE tests)
  catch_discover_tests(mediaqc_tests)
endif()

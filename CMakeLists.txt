cmake_minimum_required(VERSION 3.28)
project(relay_inspect LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(FFMPEG REQUIRED IMPORTED_TARGET
  libavformat libavcodec libavutil libswresample)

find_package(Catch2 3 REQUIRED)

add_library(relay_core
  src/version.cpp
  src/transport_reorder.cpp
  src/caption_parser.cpp
  src/sha256.cpp
)
target_include_directories(relay_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/include)
target_link_libraries(relay_core PUBLIC PkgConfig::FFMPEG)
target_compile_options(relay_core PRIVATE -Wall -Wextra)

add_executable(relay_inspect app/relay_inspect.cpp)
target_link_libraries(relay_inspect PRIVATE relay_core)

enable_testing()
add_executable(pipeline_tests tests/pipeline_tests.cpp)
target_link_libraries(pipeline_tests PRIVATE relay_core Catch2::Catch2WithMain)
add_test(NAME pipeline_tests COMMAND pipeline_tests)

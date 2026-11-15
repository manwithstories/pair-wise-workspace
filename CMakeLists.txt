cmake_minimum_required(VERSION 3.20)
project(smx LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "" FORCE)
endif()

set(CMAKE_POLICY_VERSION_MINIMUM 3.5 CACHE STRING "" FORCE)
set(FT_DISABLE_ZLIB TRUE CACHE BOOL "" FORCE)
set(FT_DISABLE_BZIP2 TRUE CACHE BOOL "" FORCE)
set(FT_DISABLE_PNG TRUE CACHE BOOL "" FORCE)
set(FT_DISABLE_HARFBUZZ TRUE CACHE BOOL "" FORCE)
set(FT_DISABLE_BROTLI TRUE CACHE BOOL "" FORCE)
set(BUILD_SHARED_LIBS OFF CACHE BOOL "" FORCE)
add_subdirectory(vendor/freetype EXCLUDE_FROM_ALL)

add_library(smx_static STATIC
  src/ttf_synth.cpp
  src/utf8.cpp
  src/font_face.cpp
  src/break_opp.cpp
  src/metrics.cpp
  src/layout.cpp
  src/cue_parse.cpp
  src/report.cpp
  src/selftest.cpp
)
set_target_properties(smx_static PROPERTIES OUTPUT_NAME smx)
target_include_directories(smx_static PUBLIC src)
target_link_libraries(smx_static PUBLIC freetype)
target_compile_options(smx_static PRIVATE -Wall -Wextra -Wpedantic)

add_executable(smx src/main.cpp)
target_link_libraries(smx PRIVATE smx_static)
target_compile_options(smx PRIVATE -Wall -Wextra -Wpedantic)

add_executable(smx_selftest tests/selftest_main.cpp)
target_link_libraries(smx_selftest PRIVATE smx_static)
target_compile_options(smx_selftest PRIVATE -Wall -Wextra -Wpedantic)

enable_testing()
add_test(NAME smx_selftest COMMAND smx_selftest)

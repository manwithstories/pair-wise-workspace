cmake_minimum_required(VERSION 3.24)
project(lutgrade VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE Release CACHE STRING "Build type" FORCE)
endif()

find_package(Threads REQUIRED)

set(LUTGRADE_CORE_SOURCES
  src/image_io.cpp
  src/lut_parser.cpp
  src/color_matrix.cpp
  src/interpolator.cpp
  src/renderer_scalar.cpp
  src/renderer_tiled.cpp
  src/statistics.cpp
  src/fixture.cpp
  src/cli.cpp
  src/report.cpp
  src/commands.cpp
  src/compare.cpp
)

add_library(lutgrade_core STATIC ${LUTGRADE_CORE_SOURCES})
target_include_directories(lutgrade_core PUBLIC
  ${CMAKE_CURRENT_SOURCE_DIR}/src
  ${CMAKE_CURRENT_SOURCE_DIR}/.third_party/CLI)
target_link_libraries(lutgrade_core PUBLIC Threads::Threads)

add_executable(lutgrade src/main.cpp)
target_link_libraries(lutgrade PRIVATE lutgrade_core)

if(CMAKE_CXX_COMPILER_ID MATCHES "Clang|GNU")
  target_compile_options(lutgrade_core PRIVATE -Wall -Wextra -Wpedantic)
  target_compile_options(lutgrade PRIVATE -Wall -Wextra -Wpedantic)
  # CLI11's Encoding header uses std::codecvt, deprecated in C++17 but still
  # the only portable converter in the vendored release. Silence it here only;
  # our own sources keep full warnings.
  target_compile_options(lutgrade PRIVATE -Wno-deprecated-declarations)
endif()

option(LUTGRADE_BUILD_TESTS "Build integration tests" ON)
if(LUTGRADE_BUILD_TESTS)
  enable_testing()
  add_executable(lutgrade_tests
    tests/integration.cpp
    tests/render_tests.cpp
    tests/cli_tests.cpp
    ${CMAKE_CURRENT_SOURCE_DIR}/.third_party/Catch2/catch_amalgamated.cpp)
  target_include_directories(lutgrade_tests PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/.third_party/Catch2)
  target_link_libraries(lutgrade_tests PRIVATE lutgrade_core)
  target_compile_definitions(lutgrade_tests PRIVATE
    LUTGRADE_BINARY="$<TARGET_FILE:lutgrade>")
  add_dependencies(lutgrade_tests lutgrade)
  if(CMAKE_CXX_COMPILER_ID MATCHES "Clang|GNU")
    target_compile_options(lutgrade_tests PRIVATE -Wall -Wextra -Wpedantic)
  endif()
  add_test(NAME integration COMMAND lutgrade_tests)
endif()

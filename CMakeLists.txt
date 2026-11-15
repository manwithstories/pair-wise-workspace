cmake_minimum_required(VERSION 3.20)

project(lutgrade VERSION 1.0.0 LANGUAGES CXX)

find_package(Threads REQUIRED)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(CMAKE_CXX_COMPILER_ID MATCHES "GNU|Clang")
  add_compile_options(-Wall -Wextra -Wpedantic)
endif()

add_library(lutgrade_core STATIC
  src/cube_parser.cpp
  src/color_matrix.cpp
  src/image_io.cpp
  src/render_scalar.cpp
  src/render_tiled.cpp
  src/histogram.cpp
  src/fixture_gen.cpp
)

target_include_directories(lutgrade_core PUBLIC src)
target_link_libraries(lutgrade_core PUBLIC Threads::Threads)

add_executable(lutgrade src/main.cpp)
target_link_libraries(lutgrade PRIVATE lutgrade_core)

enable_testing()

add_executable(lutgrade_smoke tests/smoke.cpp)
target_link_libraries(lutgrade_smoke PRIVATE lutgrade_core)

add_test(NAME smoke
  COMMAND lutgrade_smoke $<TARGET_FILE:lutgrade> ${CMAKE_CURRENT_BINARY_DIR}/smoke-work)
set_tests_properties(smoke PROPERTIES TIMEOUT 60)


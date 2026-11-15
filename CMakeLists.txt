cmake_minimum_required(VERSION 3.27)
project(sublay CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FREETYPE REQUIRED IMPORTED_TARGET freetype2)
pkg_check_modules(HARFBUZZ REQUIRED IMPORTED_TARGET harfbuzz)

add_library(sublay_core
  src/seg.cpp
  src/parse.cpp
  src/font.cpp
  src/break.cpp
  src/measure.cpp
  src/export.cpp
)
target_include_directories(sublay_core PUBLIC include)
target_link_libraries(sublay_core PUBLIC PkgConfig::FREETYPE PkgConfig::HARFBUZZ)
target_compile_options(sublay_core PRIVATE -Wall -Wextra)

add_executable(sublay src/cli.cpp)
target_link_libraries(sublay PRIVATE sublay_core)
find_package(Threads REQUIRED)
target_link_libraries(sublay PRIVATE Threads::Threads)

enable_testing()
add_test(NAME selftest COMMAND sublay selftest)

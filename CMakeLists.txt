cmake_minimum_required(VERSION 3.20)
project(mp4pack VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(mp4pack_core STATIC
    src/json.cpp
    src/byte_reader.cpp
    src/box_tree.cpp
    src/compat_check.cpp
    src/init_parse.cpp
    src/repacker.cpp
    src/report_writer.cpp
    src/segment_index.cpp
    src/util.cpp
)
target_include_directories(mp4pack_core PUBLIC src)
target_compile_options(mp4pack_core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(mp4pack src/main.cpp)
target_link_libraries(mp4pack PRIVATE mp4pack_core)
target_compile_options(mp4pack PRIVATE -Wall -Wextra -Wpedantic)

find_package(Python3 COMPONENTS Interpreter REQUIRED)
enable_testing()

add_test(NAME acceptance
    COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/tests/run_checks.sh
            $<TARGET_FILE:mp4pack>
            ${Python3_EXECUTABLE}
            ${CMAKE_CURRENT_SOURCE_DIR}/scripts/gen_fixtures.py)

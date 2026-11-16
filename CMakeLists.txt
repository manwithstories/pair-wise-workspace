cmake_minimum_required(VERSION 3.16)

project(h264gop LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(h264core STATIC
    src/bitreader.cpp
    src/exp_golomb.cpp
    src/gop_builder.cpp
    src/nal_scanner.cpp
    src/pps_parser.cpp
    src/report.cpp
    src/slice_parser.cpp
    src/sps_parser.cpp
)
target_include_directories(h264core PUBLIC src)

set(H264_FILESYSTEM_LIBRARY "")
if(CMAKE_CXX_COMPILER_ID STREQUAL "GNU" AND
   CMAKE_CXX_COMPILER_VERSION VERSION_LESS 9.1)
    set(H264_FILESYSTEM_LIBRARY stdc++fs)
endif()

add_executable(h264gop src/main.cpp)
target_link_libraries(h264gop PRIVATE h264core ${H264_FILESYSTEM_LIBRARY})

add_executable(gen_fixtures tools/gen_fixtures.cpp)
target_link_libraries(gen_fixtures PRIVATE ${H264_FILESYSTEM_LIBRARY})

add_executable(h264_unit_tests tests/unit_tests.cpp)
target_link_libraries(h264_unit_tests PRIVATE h264core)

if(CMAKE_CXX_COMPILER_ID MATCHES "GNU|Clang")
    target_compile_options(h264core PRIVATE -Wall -Wextra -Wpedantic)
    target_compile_options(h264gop PRIVATE -Wall -Wextra -Wpedantic)
    target_compile_options(gen_fixtures PRIVATE -Wall -Wextra -Wpedantic)
    target_compile_options(h264_unit_tests PRIVATE -Wall -Wextra -Wpedantic)
endif()

enable_testing()
add_test(NAME unit_tests COMMAND h264_unit_tests)
add_test(NAME end_to_end
    COMMAND ${CMAKE_COMMAND} -E env
        H264GOP=$<TARGET_FILE:h264gop>
        GEN_FIXTURES=$<TARGET_FILE:gen_fixtures>
        bash ${CMAKE_SOURCE_DIR}/tests/run_tests.sh)
set_tests_properties(end_to_end PROPERTIES TIMEOUT 60)

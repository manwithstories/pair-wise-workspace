cmake_minimum_required(VERSION 3.20)

project(fmp4guard LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(fmp4core
    src/box_reader.cpp
    src/box_tree.cpp
    src/init_segment.cpp
    src/fragment_index.cpp
    src/validate.cpp
    src/json.cpp
    src/report.cpp
    src/compare.cpp
    src/repack.cpp
)
target_include_directories(fmp4core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/src)
target_compile_options(fmp4core PRIVATE -Wall -Wextra -Wpedantic)

add_executable(fmp4gen
    tools/mkfixture.cpp
    tools/fixture_boxes.cpp
)
target_link_libraries(fmp4gen PRIVATE fmp4core)

add_executable(fmp4tool src/main.cpp)
target_link_libraries(fmp4tool PRIVATE fmp4core)

include(CTest)
if(BUILD_TESTING)
    add_executable(core_tests tests/core_tests.cpp)
    target_link_libraries(core_tests PRIVATE fmp4core)
    add_test(NAME core_tests COMMAND core_tests)

    add_test(
        NAME integration_tests
        COMMAND ${CMAKE_COMMAND}
            -DGEN=$<TARGET_FILE:fmp4gen>
            -DTOOL=$<TARGET_FILE:fmp4tool>
            -DWORK=${CMAKE_CURRENT_BINARY_DIR}/test-work
            -P ${CMAKE_CURRENT_SOURCE_DIR}/tests/integration.cmake
    )
endif()



cmake_minimum_required(VERSION 3.20)

project(subtitle_audit VERSION 0.1.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

list(PREPEND CMAKE_PREFIX_PATH "${CMAKE_SOURCE_DIR}/vendor/install")
if(EXISTS "${CMAKE_SOURCE_DIR}/vendor/install/lib/pkgconfig")
    set(ENV{PKG_CONFIG_PATH}
        "${CMAKE_SOURCE_DIR}/vendor/install/lib/pkgconfig:$ENV{PKG_CONFIG_PATH}")
endif()

find_package(PkgConfig REQUIRED)
pkg_check_modules(LIBASS REQUIRED IMPORTED_TARGET libass>=0.17.1)
pkg_check_modules(FREETYPE REQUIRED IMPORTED_TARGET freetype2>=2.13.0)
pkg_check_modules(HARFBUZZ REQUIRED IMPORTED_TARGET harfbuzz>=8.0.0)
pkg_check_modules(FONTCONFIG REQUIRED IMPORTED_TARGET fontconfig)

add_executable(subtitle-audit
    src/main.cpp
    src/ass_doc.cpp
    src/style_chain.cpp
    src/tag_lexer.cpp
    src/tag_effect.cpp
    src/run_split.cpp
    src/digest.cpp
    src/layout_probe.cpp
    tools/gen_fixtures.cpp
    src/report.cpp
    src/command_utils.cpp
    src/batch_command.cpp
)

target_include_directories(subtitle-audit PRIVATE include tools)
target_link_libraries(subtitle-audit PRIVATE
    PkgConfig::LIBASS
    PkgConfig::FREETYPE
    PkgConfig::HARFBUZZ
    PkgConfig::FONTCONFIG
)

if(MSVC)
    target_compile_options(subtitle-audit PRIVATE /W4 /utf-8)
else()
    target_compile_options(subtitle-audit PRIVATE -Wall -Wextra -Wpedantic)
endif()

install(TARGETS subtitle-audit RUNTIME DESTINATION bin)

include(CTest)
find_package(Python3 COMPONENTS Interpreter QUIET)
if(BUILD_TESTING AND Python3_Interpreter_FOUND)
    add_test(NAME smoke COMMAND sh "${CMAKE_SOURCE_DIR}/tests/smoke.sh")
    add_test(NAME edge-cases COMMAND "${Python3_EXECUTABLE}"
             "${CMAKE_SOURCE_DIR}/tests/edge_cases.py")
endif()

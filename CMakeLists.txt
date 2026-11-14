cmake_minimum_required(VERSION 3.20)
project(subqc VERSION 0.1.0 LANGUAGES C CXX)
if(POLICY CMP0135)
  cmake_policy(SET CMP0135 NEW)
endif()

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(PkgConfig REQUIRED)
pkg_check_modules(FREETYPE REQUIRED IMPORTED_TARGET freetype2>=2.13)
pkg_check_modules(HARFBUZZ REQUIRED IMPORTED_TARGET harfbuzz>=8.3)
pkg_check_modules(FRIBIDI REQUIRED IMPORTED_TARGET fribidi)

include(ExternalProject)
set(LIBASS_VERSION 0.17.1)
set(LIBASS_PREFIX "${CMAKE_BINARY_DIR}/libass-install")
set(LIBASS_ARCHIVE "${CMAKE_SOURCE_DIR}/vendor/libass-${LIBASS_VERSION}.tar.xz")
file(MAKE_DIRECTORY "${LIBASS_PREFIX}/include")

ExternalProject_Add(libass_external
  URL "${LIBASS_ARCHIVE}"
  URL_HASH SHA256=f0da0bbfba476c16ae3e1cfd862256d30915911f7abaa1b16ce62ee653192784
  PREFIX "${CMAKE_BINARY_DIR}/libass-build"
  INSTALL_DIR "${LIBASS_PREFIX}"
  CONFIGURE_COMMAND ${CMAKE_COMMAND} -E env lt_cv_sys_max_cmd_len=65536
    sh <SOURCE_DIR>/configure --prefix=<INSTALL_DIR>
    --disable-shared --enable-static --disable-fontconfig
    --disable-coretext --disable-libunibreak --disable-dependency-tracking
    --disable-require-system-font-provider
  BUILD_COMMAND make -j4
  INSTALL_COMMAND make install
  BUILD_IN_SOURCE 1
  BUILD_BYPRODUCTS "${LIBASS_PREFIX}/lib/libass.a"
)

add_library(libass::libass STATIC IMPORTED GLOBAL)
set_target_properties(libass::libass PROPERTIES
  IMPORTED_LOCATION "${LIBASS_PREFIX}/lib/libass.a"
  INTERFACE_INCLUDE_DIRECTORIES "${LIBASS_PREFIX}/include"
  INTERFACE_LINK_LIBRARIES "PkgConfig::FREETYPE;PkgConfig::HARFBUZZ;PkgConfig::FRIBIDI;m"
)
add_dependencies(libass::libass libass_external)

add_executable(subqc
  src/cli.cpp
  src/ass_reader.cpp
  src/style_chain.cpp
  src/inline_tags.cpp
  src/event_model.cpp
  src/render_probe.cpp
  src/metrics.cpp
  src/mini_font.cpp
  src/fixture_gen.cpp
)
target_include_directories(subqc PRIVATE include vendor)
target_link_libraries(subqc PRIVATE libass::libass)
if(APPLE)
  target_link_libraries(subqc PRIVATE iconv)
endif()
target_compile_options(subqc PRIVATE -Wall -Wextra -Wpedantic)

install(TARGETS subqc RUNTIME DESTINATION bin)

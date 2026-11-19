# Minimal Catch2 test discovery (equivalent to the CMake-bundled Catch module).
# Defined here because some Homebrew Catch2 installs do not ship it.
function(catch_discover_tests TARGET)
  set(options)
  set(oneValueArgs TEST_PREFIX WORKING_DIRECTORY)
  set(multiValueArgs TEST_SPEC EXTRA_ARGS)
  cmake_parse_arguments(CD "${options}" "${oneValueArgs}" "${multiValueArgs}" ${ARGN})
  if(NOT CD_TEST_PREFIX)
    set(CD_TEST_PREFIX "")
  endif()
  if(NOT CD_WORKING_DIRECTORY)
    set(CD_WORKING_DIRECTORY "${CMAKE_CURRENT_BINARY_DIR}")
  endif()
  set(ctest_file "${CMAKE_CURRENT_BINARY_DIR}/${TARGET}_tests.cmake")
  file(GENERATE OUTPUT "${ctest_file}"
    CONTENT "
add_test(NAME ${TARGET}_list COMMAND ${TARGET} --list-tests --verbosity quiet)
set_tests_properties(${TARGET}_list PROPERTIES WORKING_DIRECTORY \"${CD_WORKING_DIRECTORY}\")
")
endfunction()

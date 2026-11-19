# End-to-end acceptance test driven through the built CLI.  Mirrors the
# documented acceptance commands without depending on the caller's shell or
# on a pre-existing /tmp/archive-fixtures.
if(NOT DEFINED BINARY OR NOT DEFINED WORK_DIR)
  message(FATAL_ERROR "BINARY and WORK_DIR are required")
endif()

file(REMOVE_RECURSE "${WORK_DIR}")
file(MAKE_DIRECTORY "${WORK_DIR}")
set(manifest "${WORK_DIR}/archive.json")
set(result "${WORK_DIR}/result.json")
set(output "${WORK_DIR}/out")

function(run_cli expected_exit)
  execute_process(
    COMMAND "${BINARY}" ${ARGN}
    RESULT_VARIABLE exit_code
    OUTPUT_VARIABLE stdout
    ERROR_VARIABLE stderr)
  if(NOT exit_code EQUAL expected_exit)
    message(FATAL_ERROR
      "command failed: ${BINARY} ${ARGN}\nexit=${exit_code} expected=${expected_exit}\n"
      "stdout:\n${stdout}\nstderr:\n${stderr}")
  endif()
  set(LAST_STDOUT "${stdout}" PARENT_SCOPE)
  set(LAST_STDERR "${stderr}" PARENT_SCOPE)
endfunction()

function(expect_contains haystack needle)
  string(FIND "${haystack}" "${needle}" position)
  if(position EQUAL -1)
    message(FATAL_ERROR "expected '${needle}' in output:\n${haystack}")
  endif()
endfunction()

run_cli(0 generate --out "${WORK_DIR}" --include-damaged)
expect_contains("${LAST_STDOUT}" "damaged=included")

run_cli(0 inspect --manifest "${manifest}")
expect_contains("${LAST_STDOUT}" "manifest_roundtrip=100%")
expect_contains("${LAST_STDOUT}" "keyframe_hits=19/19")

run_cli(0 restore --manifest "${manifest}" --rate 48000 --layout 5.1
        --loudness -23 --json "${result}" --out "${output}")
expect_contains("${LAST_STDOUT}" "manifest_roundtrip=100%")
expect_contains("${LAST_STDOUT}" "keyframe_hits=19/19")
expect_contains("${LAST_STDOUT}" "segment_hash_match=4/4")
expect_contains("${LAST_STDOUT}" "audio_rate_out=48000")
expect_contains("${LAST_STDOUT}" "layout_out=5.1")
expect_contains("${LAST_STDOUT}" "loudness_lufs=-23.0")
expect_contains("${LAST_STDOUT}" "quarantined_ok=6")
# Damaged samples must surface an offset and an error code on stderr.
expect_contains("${LAST_STDERR}" "code=E_INDEX_DUPLICATE")
expect_contains("${LAST_STDERR}" "offset=0")

run_cli(0 verify --manifest "${manifest}" --result "${result}")
expect_contains("${LAST_STDOUT}" "segment_hash_match=4/4")
expect_contains("${LAST_STDOUT}" "quarantined_ok=6/6")

# A second restore over the same inputs must reproduce every hash.
run_cli(0 restore --manifest "${manifest}" --rate 48000 --layout 5.1
        --loudness -23 --json "${result}.2" --out "${output}")
file(READ "${result}" first)
file(READ "${result}.2" second)
string(REGEX MATCHALL "\"segment_hash\": \"[0-9a-f]+\"" first_hashes "${first}")
string(REGEX MATCHALL "\"segment_hash\": \"[0-9a-f]+\"" second_hashes "${second}")
if(NOT first_hashes STREQUAL second_hashes)
  message(FATAL_ERROR "segment hashes are not reproducible across runs")
endif()

# Tampering with a delivered payload must be caught by verify.
file(GLOB payloads "${output}/*/*.wav")
list(GET payloads 0 victim)
file(APPEND "${victim}" "tampered")
run_cli(2 verify --manifest "${manifest}" --result "${result}")
expect_contains("${LAST_STDERR}" "code=E_HASH_MISMATCH")

# Usage errors exit 1 and print to stderr.
run_cli(1 generate)
expect_contains("${LAST_STDERR}" "code=E_USAGE")
run_cli(1 restore --manifest "${manifest}" --rate nope)
expect_contains("${LAST_STDERR}" "code=E_USAGE")
run_cli(1 unknown-command)
expect_contains("${LAST_STDERR}" "code=E_USAGE")

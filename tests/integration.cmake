if(NOT DEFINED GEN OR NOT DEFINED TOOL OR NOT DEFINED WORK)
    message(FATAL_ERROR "GEN, TOOL, and WORK must be defined")
endif()

file(REMOVE_RECURSE "${WORK}")
file(MAKE_DIRECTORY "${WORK}")

macro(run_expect expected)
    execute_process(
        COMMAND ${ARGN}
        RESULT_VARIABLE actual
        OUTPUT_VARIABLE output
        ERROR_VARIABLE error_output
    )
    if(NOT "${actual}" STREQUAL "${expected}")
        message(FATAL_ERROR "expected exit ${expected}, got ${actual}: ${ARGN}\n${output}\n${error_output}")
    endif()
    set(RUN_OUTPUT "${output}")
endmacro()

function(require_contains text needle label)
    string(FIND "${text}" "${needle}" position)
    if(position EQUAL -1)
        message(FATAL_ERROR "missing ${label}: ${needle}\nIn: ${text}")
    endif()
endfunction()

run_expect(0 "${GEN}" --out "${WORK}/a" --fragments 12 --drop 5,9 --dup 3
           --tfdt mixed --seed 7)
run_expect(1 "${TOOL}" validate "${WORK}/a/input.mp4" --json "${WORK}/before.json")
file(READ "${WORK}/before.json" before)
require_contains("${before}" "\"fragments\":12" "twelve physical fragments")
require_contains("${before}" "\"contiguous\":false" "broken sequence")
require_contains("${before}" "\"duplicate\":[3]" "duplicate sequence 3")
require_contains("${before}" "\"missing\":[5,9]" "missing sequences 5 and 9")
require_contains("${before}" "\"deviation_max_ms\":" "track millisecond deviation")

run_expect(0 "${TOOL}" repack "${WORK}/a/input.mp4" -o "${WORK}/repacked.mp4"
           --tfdt-version 1 --renumber)
run_expect(0 "${TOOL}" validate "${WORK}/repacked.mp4" --json "${WORK}/after.json")
file(READ "${WORK}/after.json" after)
require_contains("${after}" "\"contiguous\":true" "repaired sequence")
require_contains("${after}" "\"duplicate\":[]" "duplicates removed")
require_contains("${after}" "\"missing\":[]" "gaps removed")

run_expect(0 "${TOOL}" compare "${WORK}/before.json" "${WORK}/after.json")
require_contains("${RUN_OUTPUT}" "\"fragments_before\":12" "compare before count")
require_contains("${RUN_OUTPUT}" "\"fragments_after\":12" "compare after count")
require_contains("${RUN_OUTPUT}" "\"contiguous_after\":true" "compare contiguous after")
require_contains("${RUN_OUTPUT}" "\"duplicate_after\":[]" "compare duplicates after")
require_contains("${RUN_OUTPUT}" "\"missing_after\":[]" "compare gaps after")
require_contains("${RUN_OUTPUT}" "\"tracks\":[" "compare tracks")

run_expect(0 "${TOOL}" repack "${WORK}/repacked.mp4" -o "${WORK}/repacked2.mp4"
           --tfdt-version 1 --renumber)
execute_process(
    COMMAND "${CMAKE_COMMAND}" -E compare_files "${WORK}/repacked.mp4" "${WORK}/repacked2.mp4"
    RESULT_VARIABLE compare_result
)
if(NOT compare_result EQUAL 0)
    message(FATAL_ERROR "second repack is not byte-identical")
endif()

run_expect(0 "${GEN}" --out "${WORK}/negative" --fragments 4 --negative-offset
           --tfdt mixed --trun mixed)
run_expect(0 "${TOOL}" validate "${WORK}/negative/input.mp4")

run_expect(0 "${GEN}" --out "${WORK}/large" --fragments 4 --large-mdat --tfdt mixed)
run_expect(0 "${TOOL}" validate "${WORK}/large/input.mp4")

run_expect(0 "${GEN}" --out "${WORK}/reordered" --fragments 4 --reorder)
run_expect(1 "${TOOL}" validate "${WORK}/reordered/input.mp4" --json "${WORK}/reordered.json")
file(READ "${WORK}/reordered.json" reordered_json)
require_contains("${reordered_json}" "\"out_of_order\":[1]" "out-of-order sequence")

run_expect(0 "${GEN}" --out "${WORK}/mismatch" --fragments 4 --timescale 48000)
run_expect(1 "${TOOL}" validate "${WORK}/mismatch/input.mp4" --json "${WORK}/init.json"
           --init "${WORK}/a/input.mp4")
file(READ "${WORK}/init.json" init_json)
require_contains("${init_json}" "\"checked\":true" "init comparison enabled")
require_contains("${init_json}" "\"compatible\":false" "init mismatch detected")
require_contains("${init_json}" "timescale" "timescale mismatch field")

message(STATUS "integration tests passed")


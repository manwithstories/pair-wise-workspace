@timeline v2
fps 24000/1001

track 1
# group 1: two-cue trim, s01b keeps priority
cue s01a 0 34 pri 1 "subtitle 1 first"
cue s01b 24 58 pri 5 "subtitle 1 second"

# group 2: two-cue trim, s02b keeps priority
cue s02a 72 105 pri 1 "subtitle 2 first"
cue s02b 96 129 pri 5 "subtitle 2 second"

# group 3: two-cue trim, s03b keeps priority
cue s03a 144 177 pri 1 "subtitle 3 first"
cue s03b 168 201 pri 5 "subtitle 3 second"

# group 4: two-cue trim, s04b keeps priority
cue s04a 216 249 pri 1 "subtitle 4 first"
cue s04b 240 273 pri 5 "subtitle 4 second"

# group 5: two-cue trim, s05b keeps priority
cue s05a 288 321 pri 1 "subtitle 5 first"
cue s05b 312 345 pri 5 "subtitle 5 second"

# group 6: touching endpoints, half-open -> no overlap
cue s06a 360 384 pri 1 "subtitle 6 first"
cue s06b 384 408 pri 5 "subtitle 6 second"

cue f01 441 456 "filler one"

track 2
# group 7: two-cue trim, s07b keeps priority
cue s07a 0 34 pri 1 "subtitle 7 first"
cue s07b 24 58 pri 5 "subtitle 7 second"

# group 8: two-cue trim, s08b keeps priority
cue s08a 72 105 pri 1 "subtitle 8 first"
cue s08b 96 129 pri 5 "subtitle 8 second"

# group 9: two-cue trim, s09b keeps priority
cue s09a 144 177 pri 1 "subtitle 9 first"
cue s09b 168 201 pri 5 "subtitle 9 second"

# group 10: truncated remainder is 300 ms -> drop
cue s10a 216 245 pri 9 "kept whole"
cue s10b 230 252 pri 2 "short remainder"

cue f02 264 278 "filler two"
cue f03 288 304 "filler three"
cue f04 316 333 "filler four"
cue f05 348 362 "filler five"
cue f06 379 396 "filler six"
cue f08 436 451 "filler eight"

# transition avoidance demonstrations (track 2)
transition cut 671 671
cue d01 652 691 "crosses a cut"
transition dissolve 707 731
cue d02 691 738 "crosses a dissolve"
transition wipe 767 791
cue d03 762 798 "crosses a wipe and shifts"
@end

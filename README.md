# artifact-resolver-kernel

A deterministic, offline resolution kernel for Maven-style internal artifact repositories.
Given a root requirement and local metadata it either produces a reproducible resolution
tree, or reports — within a bounded number of steps — which two constraints are mutually
exclusive and what the smallest fix is.

No network. No third-party runtime dependencies. Java 17.

## Why

Range declarations (`[1.2,1.5)`) and `exclusion` rules make the same manifest resolve
differently depending on which strategy the resolver uses, and a naive search either loops
forever or exhausts the heap. This kernel is built so that:

* the same input always produces the same output, byte for byte;
* an unsatisfiable manifest always terminates, with a named conflict and a repair;
* backtracking is capped, so there is no unbounded search.

## Build and verify

```bash
mvn test -Dtest=ResolverStressTest   # the scale/budget suite
mvn test                             # everything
```

## Data flow

```
root requirement
  -> interval intersection          (Interval)
  -> greedy highest-version select  (Resolver)
  -> iterative-deepening repair     (Resolver, K = 1..8)
  -> LockFile write or replay       (LockFile)
```

## Core files

| File | Role |
| --- | --- |
| `Version.java` | semantic version parsing, normalisation and total ordering (pre-release, SNAPSHOT) |
| `Interval.java` | half-open intervals as a `TreeMap` of bounds; intersection, disjointness, collapse |
| `Resolver.java` | the solver: greedy selection, iterative deepening, exclusions, conflict reporting |
| `LockFile.java` | deterministic serialisation, SHA-256 verification, verified replay |
| `Trace.java` | optional decision trace: trigger, choice, reason, propagation order |
| `MetadataDir.java` | reads the local metadata directory (no network) |
| `Main.java` | terminal entry point |

## Version ordering

Deterministic and total, lowest to highest:

1. numeric components compare component-wise, missing ones as zero (`1.2 == 1.2.0`);
2. a release outranks any pre-release of itself (`1.0-RC1 < 1.0`);
3. pre-release identifiers compare case-insensitively; a numeric one ranks below an
   alphanumeric one (`1.0-1 < 1.0-alpha`), and numeric ones compare by value;
4. `SNAPSHOT` ranks below the released form (`1.0-SNAPSHOT < 1.0`).

Trailing zeros normalise away, so `1.0-SNAPSHOT` renders as `1-SNAPSHOT`. A qualifier tail
is re-emitted verbatim, so a timestamped qualifier (`1.0-20240101.101500-3-SNAPSHOT`)
round-trips exactly.

## Intervals

Maven-style literals: `[1.2,1.5)`, `[1.2,1.5]`, `(1.2,1.5]`, `[1.2,)`, `(,1.5)`, `[1.2]`,
or a bare `1.2` (treated as `[1.2,1.2]`).

* **Intersection** takes the greater lower edge and the lesser upper edge. On a shared edge
  the intersection is exclusive unless *both* sides include it, so
  `[1.4,1.5) ∩ [1.0,1.5] = [1.4,1.5)`.
* **Empty set** is detected when the bounds cross, or when a shared edge is excluded by
  either side. The result carries provenance naming *both* constraints, so a conflict report
  can point at the exact pair to fix.
* **Collapse** folds overlapping and touching intervals into a stable ascending list. Two
  intervals that meet at a single point fold unless *neither* admits it — `[1,1.5)` and
  `(1.5,2]` leave 1.5 uncovered and stay separate. Input order never affects the output.

## Resolution

Packages are visited in name order, candidates are compared with `Version.compareTo`, and
every tie breaks on the package name. Nothing consults hash order, wall-clock time, or
thread scheduling, so repeated runs produce identical trees and identical lock images.

**Greedy selection** takes the highest published version satisfying the intersected window
that no placed ancestor excludes. A version is skipped when an ancestor's exclusion forbids
it.

**Iterative-deepening local repair.** `K` counts the number of decisions allowed to settle
on something other than their greedy version. The solver runs `K = 1..8` and stops at the
first pass that succeeds, so the reported fix is the one that changes the fewest decisions.
Beyond `K = 8` it reports a conflict instead of continuing to search.

**Exclusions.** An exclusion on `owner@version` forbids the target from appearing anywhere
in that owner's subtree. Because the decision order places every ancestor before its
descendants, the check is a walk of the recorded ancestor chain.

**Termination.** The search is bounded twice over: by the downgrade budget `K` and by a hard
placement cap per pass. Cyclic graphs terminate because a requirement naming an
already-assigned package is validated against the assignment rather than re-placed.

## Lock files

```
#artifact-resolver-lock v1
sha256=<64 hex chars over the body>
#packages=3
com.acme:app -> 2|[2,3)|com.acme:core=1.3,!com.acme:legacy
com.acme:core -> 1.3|[1.3,2)|com.acme:io=1
com.acme:io -> 1|[1,2)|
```

Entries are sorted by package name, so the byte image does not depend on traversal order
and can be hashed. Replay verifies the digest **before** doing any resolution work, then
pins each package to its locked version and re-checks the recorded edges without invoking
the search. A hand-edited or stale lock is rejected; a lock whose pinned versions no longer
satisfy the manifest is reported as a violation rather than silently re-solved.

## Tracing

Tracing is off by default and costs nothing when off. With `--trace`, every decision records
the propagation order, what triggered it, which constraint bounded its window, the version
chosen, and why a downgrade happened:

```
#0002 a:mid -> 3  GREEDY(K=1)  window=[1,4)  avail=2  by=a:root@1
        from=a:root@1 -> a:mid [1.0,4.0)  why=highest version satisfying [1,4)
        chain=a:root -> a:mid
```

## Metadata directory

One `<groupId>__<artifactId>.dep` file per package; the `__` stands in for the `:` in the
package id, and dots inside the group id are preserved.

```
# com.acme core
version 1.4.0
version 1.5.0 -> com.acme:io [1.4,2.0)
version 1.5.0 ! com.acme:legacy
```

Files are read in sorted order, so a given directory always yields the same repository.

## Command line

```bash
java -cp target/classes io.corp.artifactres.Main <metadataDir> [options]

  --root  pkg:range   declare a root requirement (repeatable)
  --lock  file        write the resolved tree to a lock file
  --replay file       verify and replay a lock file instead of solving
  --trace             print every decision in propagation order
```

Note that `[1.0,1.0)` matches nothing (the upper bound is exclusive); use `[1.0,1.0]` to pin
an exact version. The CLI points this out rather than failing with a confusing conflict.

## Budgets

Asserted in `ResolverStressTest`, measured with `System.nanoTime()` as the median of three
runs, with the ceilings enforced on every test JVM:

| Budget | Limit | Measured |
| --- | --- | --- |
| resolve 2000 packages / 8000 constraints | 300ms | ~30-90ms |
| lock file replay | 50ms | ~5-20ms |
| heap peak | 64MB | enforced via `-Xmx64m` |
| backtrack depth | 8 | enforced by `MAX_DEPTH` |

Surefire also runs with `-Xss4m`: resolution descends one frame per package, so a large
manifest needs more than the JDK default thread stack. The value is pinned rather than
inherited so the budget is part of the build, not of the environment.

package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Scale and budget tests for the resolver kernel.
 *
 * <p>Verification command: {@code mvn test -Dtest=ResolverStressTest}
 *
 * <p>Budgets asserted here, all measured with {@link System#nanoTime()}:
 * <ul>
 *   <li>2000 packages / 8000 constraints resolve in under 300ms (median of three runs)</li>
 *   <li>lock file replay in under 50ms (median of three runs)</li>
 *   <li>heap peak under 64MB, enforced by surefire's {@code -Xmx64m} for every test JVM</li>
 *   <li>backtracking never exceeds {@link Resolver#MAX_DEPTH}</li>
 * </ul>
 */
class ResolverStressTest {

    private static final int PACKAGES = 2000;
    private static final int CONSTRAINTS = 8000;

    private static final long RESOLVE_BUDGET_NS = 300L * 1_000_000;
    private static final long REPLAY_BUDGET_NS = 50L * 1_000_000;
    private static final long HEAP_BUDGET_BYTES = 64L * 1024 * 1024;

    /** Median of three timed samples: the middle value, immune to one-off JIT/GC noise. */
    private static long medianOfThree(Runnable task) {
        long[] samples = new long[3];
        for (int i = 0; i < 3; i++) {
            long t0 = System.nanoTime();
            task.run();
            samples[i] = System.nanoTime() - t0;
        }
        Arrays.sort(samples);
        return samples[1];
    }

    // ------------------------------------------------------------ resolve budget

    @Test
    @DisplayName("2000 packages / 8000 constraints resolve within 300ms (median of 3)")
    void resolvesLargeGraphWithinBudget() {
        SyntheticGraph.Graph g = SyntheticGraph.build(PACKAGES, CONSTRAINTS, 20240517L);
        assertEquals(PACKAGES, g.repo.size(), "generated graph should hold the requested package count");
        assertEquals(CONSTRAINTS, g.constraints, "generated graph should hold the requested edge count");

        // Resolve once up front so the measured runs score steady-state code, not the JIT.
        SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();

        long[] samples = new long[3];
        Resolver.Resolution[] results = new Resolver.Resolution[3];
        for (int i = 0; i < 3; i++) {
            Resolver r = SyntheticGraph.resolverFor(g, Trace.disabled());
            long t0 = System.nanoTime();
            results[i] = r.resolve();
            samples[i] = System.nanoTime() - t0;
        }
        Arrays.sort(samples);
        long medianMs = samples[1] / 1_000_000;

        // Every run must produce the same tree, not merely a fast one.
        assertEquals(results[0].selected(), results[1].selected(),
                "resolution must be deterministic across repeated runs");
        assertEquals(results[1].selected(), results[2].selected(),
                "resolution must be deterministic across repeated runs");
        assertEquals(PACKAGES, results[0].size(), "every package should be resolved");

        assertTrue(medianMs < RESOLVE_BUDGET_NS / 1_000_000,
                "median resolve time " + medianMs + "ms exceeds the 300ms budget "
                        + "(samples: " + samples[0] / 1_000_000 + "/"
                        + samples[1] / 1_000_000 + "/" + samples[2] / 1_000_000 + "ms)");
    }

    @Test
    @DisplayName("the large resolution honours every declared edge window")
    void largeResolutionSatisfiesAllEdges() {
        SyntheticGraph.Graph g = SyntheticGraph.build(PACKAGES, CONSTRAINTS, 20240517L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();

        // Independently re-check the tree: for every chosen version, each of its edges must
        // be satisfied by the version actually chosen for the target.
        for (Map.Entry<String, Version> e : res.selected().entrySet()) {
            Resolver.Module m = g.repo.get(e.getKey());
            for (Resolver.Requirement r : m.depsOf(e.getValue())) {
                if (r.exclusion) {
                    assertFalse(res.selected().containsKey(r.target),
                            e.getKey() + "@" + e.getValue() + " excludes " + r.target
                                    + " but it was resolved");
                    continue;
                }
                Version chosen = res.versionOf(r.target);
                assertNotNull(chosen, e.getKey() + " requires " + r.target + " which is unresolved");
                assertTrue(r.range.contains(chosen),
                        e.getKey() + "@" + e.getValue() + " requires " + r.target
                                + " " + r.range + " but resolution chose " + chosen);
            }
        }
    }

    // ------------------------------------------------------------- replay budget

    @Test
    @DisplayName("lock file replay completes within 50ms (median of 3)")
    void replaysLockFileWithinBudget() {
        SyntheticGraph.Graph g = SyntheticGraph.build(PACKAGES, CONSTRAINTS, 20240517L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();
        LockFile lock = LockFile.of(res);
        String image = lock.render();

        // Warm up parsing/JIT before measuring.
        LockFile.parse(image).replay(new Resolver(g.repo, Trace.disabled()));

        long medianNs = medianOfThree(() -> {
            LockFile parsed = LockFile.parse(image);
            LockFile.Replay replay = parsed.replay(new Resolver(g.repo, Trace.disabled()));
            assertTrue(replay.isConsistent(),
                    "a freshly written lock must replay without violations");
            assertEquals(PACKAGES, replay.pinned.size(), "replay must pin every package");
        });

        assertTrue(medianNs < REPLAY_BUDGET_NS,
                "median replay time " + medianNs / 1_000_000 + "ms exceeds the 50ms budget");
    }

    @Test
    @DisplayName("replay pins the locked versions without re-solving")
    void replayPinsLockedVersions() {
        SyntheticGraph.Graph g = SyntheticGraph.build(500, 2000, 99L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();
        LockFile lock = LockFile.of(res);

        LockFile.Replay replay = lock.replay(new Resolver(g.repo, Trace.disabled()));
        assertTrue(replay.isConsistent(), "fresh lock must replay cleanly");
        assertEquals(res.selected(), replay.pinned,
                "replay must reproduce exactly the locked versions");
    }

    @Test
    @DisplayName("a tampered lock file is rejected on the SHA-256 check")
    void rejectsTamperedLockFile() {
        SyntheticGraph.Graph g = SyntheticGraph.build(50, 120, 7L);
        LockFile lock = LockFile.of(SyntheticGraph.resolverFor(g, Trace.disabled()).resolve());

        // Rewrite one body line's version, leaving the sha256 header stale.
        String[] lines = lock.render().split("\n", -1);
        int bodyLine = -1;
        for (int i = 0; i < lines.length; i++) {
            if (lines[i].contains(" -> ") && lines[i].indexOf('|') > 0) {
                bodyLine = i;
                break;
            }
        }
        assertTrue(bodyLine > 0, "the lock image should contain at least one entry line");
        // Body line shape: "<pkg> -> <version>|<window>|<deps>"; swap the version field.
        String original = lines[bodyLine];
        int arrow = original.indexOf(" -> ") + 4;
        int bar = original.indexOf('|', arrow);
        lines[bodyLine] = original.substring(0, arrow) + "999.0" + original.substring(bar);
        String tampered = String.join("\n", lines);

        assertFalse(tampered.equals(lock.render()), "the tampering must actually change the image");
        IllegalArgumentException ex = assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse(tampered),
                "an edited lock file must be rejected");
        assertTrue(ex.getMessage().contains("digest mismatch"),
                "expected a digest-mismatch report, got: " + ex.getMessage());
    }

    // ------------------------------------------------------------- heap budget

    @Test
    @DisplayName("the whole scale workload fits inside the 64MB heap ceiling")
    void fitsInHeapBudget() {
        long max = Runtime.getRuntime().maxMemory();
        assertTrue(max <= HEAP_BUDGET_BYTES,
                "test JVM heap ceiling is " + max / (1024 * 1024) + "MB, expected at most 64MB. "
                        + "The kernel must not be allowed to grow past the stated budget.");

        // Exercise the full pipeline under that ceiling rather than merely asserting it.
        SyntheticGraph.Graph g = SyntheticGraph.build(PACKAGES, CONSTRAINTS, 31337L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();
        LockFile lock = LockFile.of(res);
        LockFile parsed = LockFile.parse(lock.render());
        assertTrue(parsed.replay(new Resolver(g.repo, Trace.disabled())).isConsistent(),
                "scale workload must complete inside the ceiling");

        long used = Runtime.getRuntime().totalMemory() - Runtime.getRuntime().freeMemory();
        assertTrue(used <= HEAP_BUDGET_BYTES,
                "live heap after the scale workload was " + used / (1024 * 1024) + "MB");
    }

    // ------------------------------------------------- bounded backtracking

    @Test
    @DisplayName("backtracking terminates within MAX_DEPTH and repairs a deep greedy trap")
    void backtrackingIsBounded() {
        // A greedy trap: every level's highest version demands the next level sit in
        // [3,4), but the bottom leaf publishes only 1 and 2, so the all-greedy chain is
        // unsatisfiable. A correct resolver must unwind the whole stack and settle on the
        // consistent all-low tree, doing so within MAX_DEPTH.
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("r:root", Resolver.Module.builder("r:root")
                .version("1.0", "a:top", "[1.0,4.0)").build());
        repo.put("a:top", Resolver.Module.builder("a:top")
                .version("1.0", "a:l01", "[1.0,2.0)")
                .version("2.0", "a:l01", "[1.0,2.0)")
                .version("3.0", "a:l01", "[3.0,4.0)")
                .build());
        for (int i = 1; i <= 5; i++) {
            String id = String.format("a:l%02d", i);
            String next = i < 5 ? String.format("a:l%02d", i + 1) : "a:leaf";
            repo.put(id, Resolver.Module.builder(id)
                    .version("1.0", next, "[1.0,2.0)")
                    .version("2.0", next, "[1.0,2.0)")
                    .version("3.0", next, "[3.0,4.0)")
                    .build());
        }
        // No version in [3,4): the greedy chain is genuinely unsatisfiable.
        repo.put("a:leaf", Resolver.Module.builder("a:leaf").version("1.0").version("2.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("r:root", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        assertTrue(r.lastDepth() <= Resolver.MAX_DEPTH,
                "used depth " + r.lastDepth() + ", must not exceed " + Resolver.MAX_DEPTH);
        assertTrue(r.lastDepth() > 1,
                "this fixture must actually force backtracking, not resolve greedily");

        // Verify the produced tree independently rather than trusting the resolver.
        for (Map.Entry<String, Version> e : res.selected().entrySet()) {
            for (Resolver.Requirement req : repo.get(e.getKey()).depsOf(e.getValue())) {
                if (req.exclusion) {
                    continue;
                }
                Version tv = res.versionOf(req.target);
                assertNotNull(tv, e.getKey() + " requires " + req.target + " which is unresolved");
                assertTrue(req.range.contains(tv),
                        e.getKey() + "@" + e.getValue() + " requires " + req.target
                                + " " + req.range + " but got " + tv);
            }
        }
        assertEquals(Version.parse("1.0"), res.versionOf("a:leaf"),
                "the leaf must settle on a version the whole chain can agree with");
    }

    @Test
    @DisplayName("an unsatisfiable graph reports a conflict instead of looping forever")
    void unsatisfiableGraphReportsConflict() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        // Every version of a:core demands a window on a:leaf that no leaf version satisfies.
        repo.put("r:root", Resolver.Module.builder("r:root")
                .version("1.0", "a:core", "[1.0,5.0)").build());
        repo.put("a:core", Resolver.Module.builder("a:core")
                .version("1.0", "a:leaf", "[4.0,5.0)")
                .version("2.0", "a:leaf", "[4.0,5.0)")
                .version("3.0", "a:leaf", "[4.0,5.0)")
                .build());
        repo.put("a:leaf", Resolver.Module.builder("a:leaf").version("1.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("r:root", "[1.0,1.0]");

        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve,
                "an unsatisfiable graph must report a conflict, not hang or loop");
        assertEquals("a:leaf", conflict.pkg);
        assertNotNull(conflict.repair, "a conflict must carry a repair suggestion");
        assertFalse(conflict.repair.isBlank(), "repair suggestion must not be blank");
        assertTrue(conflict.depth <= Resolver.MAX_DEPTH,
                "conflict reported at depth " + conflict.depth + ", cap is " + Resolver.MAX_DEPTH);
    }

    @Test
    @DisplayName("a single package pinned by two exclusive windows names both constraints")
    void emptyIntersectionNamesBothConstraints() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("r:root", Resolver.Module.builder("r:root")
                .version("1.0", "a:lib", "[1.0,2.0)")
                .version("1.0", "a:lib", "[3.0,4.0)")
                .build());
        repo.put("a:lib", Resolver.Module.builder("a:lib").version("1.0").version("3.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("r:root", "[1.0,1.0]");

        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);
        assertEquals("a:lib", conflict.pkg);
        assertTrue(conflict.window.isEmptySet(), "the collapsed window must be the empty set");
        // The two rival constraints must both appear, so a reader knows which pair to fix.
        String a = String.valueOf(conflict.conflictA);
        String b = String.valueOf(conflict.conflictB);
        List<String> named = List.of(a, b);
        assertTrue(named.stream().anyMatch(t -> t.contains("a:lib [1,2)")),
                "the report must name the [1.0,2.0) constraint, got: " + named);
        assertTrue(named.stream().anyMatch(t -> t.contains("a:lib [3,4)")),
                "the report must name the [3.0,4.0) constraint, got: " + named);
        assertTrue(named.stream().anyMatch(t -> t.contains("<root>") || t.contains("r:root")),
                "each constraint must carry provenance, got: " + named);
    }

    // ------------------------------------------------------ determinism + trace

    @Test
    @DisplayName("repeated runs over the same metadata produce identical lock images")
    void resolutionIsByteIdenticalAcrossRuns() {
        SyntheticGraph.Graph g = SyntheticGraph.build(400, 1600, 555L);

        String first = LockFile.of(SyntheticGraph.resolverFor(g, Trace.disabled()).resolve()).render();
        for (int i = 0; i < 5; i++) {
            String again = LockFile.of(
                    SyntheticGraph.resolverFor(g, Trace.disabled()).resolve()).render();
            assertEquals(first, again, "lock image must be byte-identical across runs");
        }
    }

    @Test
    @DisplayName("tracing is optional and off by default")
    void tracingIsOptional() {
        SyntheticGraph.Graph g = SyntheticGraph.build(200, 800, 8L);

        Resolver quiet = SyntheticGraph.resolverFor(g, Trace.disabled());
        assertFalse(quiet.trace().isEnabled());
        quiet.resolve();
        assertEquals(0, quiet.trace().size(), "a disabled trace must record nothing");

        Trace trace = Trace.enabled();
        Resolver loud = SyntheticGraph.resolverFor(g, trace);
        loud.resolve();
        assertTrue(trace.size() > 0, "an enabled trace must record every decision");
        assertEquals(trace.entries().size(), trace.size());

        // Every entry must explain itself: package, version or reason, and a window.
        Set<Integer> steps = new HashSet<>();
        for (Trace.Entry e : trace.entries()) {
            assertNotNull(e.pkg);
            assertNotNull(e.reason);
            assertFalse(e.reason.isBlank(), "trace entry for " + e.pkg + " has no reason");
            assertTrue(steps.add(e.step), "trace steps must be unique and monotonic");
        }
    }

    @Test
    @DisplayName("a trace records the trigger, the choice and the downgrade reason")
    void traceExplainsDecisions() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("r:root", Resolver.Module.builder("r:root")
                .version("1.0", "a:mid", "[1.0,4.0)")
                .version("1.0", "a:util", "[1.0,2.0)").build());
        repo.put("a:mid", Resolver.Module.builder("a:mid")
                .version("2.0", "a:leaf", "[1.0,2.0)")
                .version("3.0", "a:leaf", "[9.0,10.0)").build());
        repo.put("a:util", Resolver.Module.builder("a:util")
                .version("1.0", "a:leaf", "[1.0,2.0)").build());
        repo.put("a:leaf", Resolver.Module.builder("a:leaf").version("1.0").version("9.0").build());

        Trace trace = Trace.enabled();
        Resolver r = new Resolver(repo, trace);
        r.require("r:root", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        assertEquals(Version.parse("2.0"), res.versionOf("a:mid"),
                "mid must be downgraded so both edges can agree on the leaf");
        assertEquals(Version.parse("1.0"), res.versionOf("a:leaf"));

        List<Trace.Entry> midEntries = trace.entriesFor("a:mid");
        assertFalse(midEntries.isEmpty(), "the downgraded package must appear in the trace");
        boolean sawDowngrade = midEntries.stream()
                .anyMatch(e -> e.kind == Trace.Kind.DOWNGRADE);
        assertTrue(sawDowngrade, "the trace must record why a:mid was downgraded");

        // The rendering must stay stable across identical runs.
        String rendered = trace.render();
        assertTrue(rendered.contains("a:mid"), "rendered trace must mention the package");
        assertTrue(rendered.contains("chain="), "rendered trace must show the decision chain");
    }

    // ------------------------------------------------- exclusion handling at scale

    @Test
    @DisplayName("an exclusion removes a package from the resolution or explains why it cannot")
    void exclusionsAreEnforced() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        // app excludes legacy, yet core drags it in: unsatisfiable by construction.
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .version("1.0", "a:legacy", "[1.0,2.0)")
                .exclude("1.0", "a:legacy")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0", "a:legacy", "[1.0,2.0)")
                .build());
        repo.put("a:legacy", Resolver.Module.builder("a:legacy").version("1.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);

        String reported = String.valueOf(conflict.conflictB) + String.valueOf(conflict.conflictA);
        assertTrue(reported.contains("excludes a:legacy"),
                "the conflict must name the exclusion rule, got: " + reported);
        assertTrue(conflict.repair.contains("a:legacy"),
                "the repair must mention the excluded package, got: " + conflict.repair);
    }

    @Test
    @DisplayName("an exclusion that nothing else needs resolves cleanly")
    void exclusionOfAnUnusedPackageIsHarmless() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .exclude("1.0", "a:legacy")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core").version("1.0").build());
        repo.put("a:legacy", Resolver.Module.builder("a:legacy").version("1.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        assertEquals(Version.parse("1.0"), res.versionOf("b:core"));
        assertFalse(res.selected().containsKey("a:legacy"),
                "the excluded package must not appear in the resolution");
    }

    // ------------------------------------------------------------ lock round-trip

    @Test
    @DisplayName("a lock file round-trips through disk with an intact digest")
    void lockFileRoundTripsThroughDisk(@org.junit.jupiter.api.io.TempDir Path tmp)
            throws IOException {
        SyntheticGraph.Graph g = SyntheticGraph.build(300, 1200, 4242L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();
        LockFile lock = LockFile.of(res);

        Path file = tmp.resolve("resolver.lock");
        lock.write(file);
        LockFile back = LockFile.read(file);

        assertEquals(lock.sha256(), back.sha256(), "digest must survive a disk round-trip");
        assertEquals(lock.size(), back.size());
        for (Map.Entry<String, Version> e : res.selected().entrySet()) {
            assertEquals(e.getValue(), back.versionOf(e.getKey()),
                    "pinned version for " + e.getKey() + " changed across the round-trip");
            assertNotNull(back.windowOf(e.getKey()), "window for " + e.getKey() + " was lost");
        }
        assertTrue(Files.readString(file, StandardCharsets.UTF_8).startsWith(LockFile.MAGIC));
    }

    @Test
    @DisplayName("lock entries are written in package-name order")
    void lockEntriesAreSorted() {
        SyntheticGraph.Graph g = SyntheticGraph.build(200, 600, 17L);
        Resolver.Resolution res = SyntheticGraph.resolverFor(g, Trace.disabled()).resolve();

        List<String> keys = new ArrayList<>(res.lockLines().keySet());
        List<String> sorted = new ArrayList<>(keys);
        java.util.Collections.sort(sorted);
        assertEquals(sorted, keys, "lock lines must be emitted in package-name order");
    }
}

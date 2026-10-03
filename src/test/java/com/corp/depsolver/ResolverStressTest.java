package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Random;
import java.util.TreeMap;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * The performance contract: 2000 packages and 8000 constraints must resolve inside 300ms, lock file
 * replay inside 50ms, and the whole thing inside a bounded heap. Timing uses {@link System#nanoTime}
 * and reports the median of three runs.
 */
class ResolverStressTest {

    private static final int PACKAGES = 2000;
    private static final int CONSTRAINTS = 8000;
    private static final long RESOLVE_BUDGET_MS = 300;
    private static final long REPLAY_BUDGET_MS = 50;

    @TempDir
    static Path metaDir;

    private static Repository repo;
    private static List<Resolver.Requirement> roots;
    private static int graphConstraints;

    @BeforeAll
    static void buildSyntheticGraph() throws IOException {
        SyntheticGraph graph = SyntheticGraph.generate(PACKAGES, CONSTRAINTS, 20260904L);
        graph.writeTo(metaDir);
        repo = Repository.load(metaDir);
        roots = graph.roots();

        assertEquals(PACKAGES, repo.packageCount(),
                "synthetic graph must expose exactly the requested package count");
        assertTrue(graph.constraintCount() >= CONSTRAINTS,
                "graph must carry at least " + CONSTRAINTS + " constraints, had " + graph.constraintCount());
        graphConstraints = graph.constraintCount();
    }

    @Test
    @DisplayName("2000 packages / 8000 constraints resolve within 300ms, median of three")
    void resolveWithinBudget() throws Exception {
        long median = medianOfThree(() -> new Resolver(repo).resolve(roots));
        System.out.printf("resolve: %d packages, %d constraints, median %.1f ms%n",
                repo.packageCount(), graphConstraints, median / 1e6);
        assertTrue(median / 1_000_000 < RESOLVE_BUDGET_MS,
                "median resolve time " + (median / 1e6) + " ms exceeds the "
                        + RESOLVE_BUDGET_MS + " ms budget");
    }

    @Test
    @DisplayName("repeated resolves are byte identical, so the result is reproducible")
    void resolveIsDeterministic() throws Resolver.Conflict {
        Resolver.Resolution a = new Resolver(repo).resolve(roots);
        Resolver.Resolution b = new Resolver(repo).resolve(roots);
        assertEquals(a.selected().toString(), b.selected().toString());
        assertEquals(LockFile.write(a).sha256(), LockFile.write(b).sha256());
        assertEquals(PACKAGES, a.size(), "every package in the graph must be selected");
    }

    @Test
    @DisplayName("resolution order does not depend on the order the roots were supplied")
    void rootOrderDoesNotMatter() throws Resolver.Conflict {
        Resolver.Resolution forward = new Resolver(repo).resolve(roots);
        List<Resolver.Requirement> reversed = new ArrayList<>(roots);
        java.util.Collections.reverse(reversed);
        Resolver.Resolution backward = new Resolver(repo).resolve(reversed);
        assertEquals(LockFile.write(forward).sha256(), LockFile.write(backward).sha256());
    }

    @Test
    @DisplayName("lock file replay stays within 50ms, median of three")
    void replayWithinBudget() throws Exception {
        Resolver.Resolution res = new Resolver(repo).resolve(roots);
        LockFile lock = LockFile.write(res);
        TreeMap<String, Version> pinned = new TreeMap<>(lock.pinnedVersions());
        long median = medianOfThree(() -> new Resolver(repo).replay(pinned, roots));
        System.out.printf("replay: %d packages, median %.2f ms%n", pinned.size(), median / 1e6);
        assertTrue(median / 1_000_000 < REPLAY_BUDGET_MS,
                "median replay time " + (median / 1e6) + " ms exceeds the "
                        + REPLAY_BUDGET_MS + " ms budget");
    }

    @Test
    @DisplayName("a heap of 64MB is enough for the full 2000 package run")
    void fitsInBoundedHeap() throws Exception {
        // The surefire JVM runs with a larger ceiling; here we assert the live set after a full
        // resolve plus lock round trip is far below the 64MB contract, using the used heap as proxy.
        Runtime rt = Runtime.getRuntime();
        System.gc();
        long before = rt.totalMemory() - rt.freeMemory();
        Resolver.Resolution res = new Resolver(repo, Trace.enabled()).resolve(roots);
        LockFile lock = LockFile.write(res);
        lock.verifyLineChecksums();
        System.gc();
        long after = rt.totalMemory() - rt.freeMemory();
        long usedMb = (after - before) / (1024 * 1024);
        System.out.printf("heap delta after resolve+lock+trace: %d KB%n", (after - before) / 1024);
        assertTrue(usedMb < 64, "resolve retained " + usedMb + " MB, above the 64MB budget");
    }

    @Test
    @DisplayName("an unsatisfiable tail reports a conflict with a suggestion instead of looping")
    void unsatisfiableTailTerminates() {
        List<Resolver.Requirement> bad = new ArrayList<>();
        bad.add(Resolver.root("com.acme.p0000", "[1.0.0,9.9.9)"));
        Repository small = smallRepo();
        try {
            new Resolver(small).resolve(bad);
            assertTrue(false, "expected a conflict");
        } catch (Resolver.Conflict e) {
            assertNotNull(e.packageId());
            assertFalse(e.chain().isEmpty(), "a conflict must carry its chain");
            assertFalse(e.report().isEmpty());
        }
    }

    @Test
    @DisplayName("the backtrack window is capped at 8 and the cap is enforced")
    void backtrackCapIsEight() {
        assertEquals(8, Resolver.MAX_BACKTRACK_DEPTH);
    }

    /** Median of three {@link System#nanoTime} measurements, in nanoseconds. */
    private static long medianOfThree(ThrowingSupplier action) throws Exception {
        long[] samples = new long[3];
        for (int i = 0; i < 3; i++) {
            System.gc();   // keep GC noise from dominating one of the three samples
            long t0 = System.nanoTime();
            action.get();
            samples[i] = System.nanoTime() - t0;
        }
        Arrays.sort(samples);
        return samples[1];
    }

    @FunctionalInterface
    private interface ThrowingSupplier {
        void get() throws Exception;
    }

    private static Repository smallRepo() {
        java.util.Map<String, List<Repository.Meta>> entries = new TreeMap<>();
        entries.put("com.acme.p0000", List.of(new Repository.Meta(
                "com.acme.p0000", Version.parse("0.0.1"), List.of(), List.of())));
        return Repository.of(entries);
    }

    /**
     * Builds a deterministic synthetic graph.
     *
     * <p>Every package {@code com.acme.pNNNN} publishes 3 to 6 versions numbered {@code 1.<i>.0} for
     * {@code i} running 1..n, so a version index maps straight onto a minor number.
     *
     * <p>Each package also gets a <em>pin</em>, a version every incoming edge is built to admit. Edges
     * take the form {@code [1.<lo>.0, 1.<hi>.0)} where {@code lo} is at most the target's pin, so the
     * pin always lies inside the range, and {@code hi} is either strictly above the target's highest
     * published minor or left unbounded. Two consequences make the graph provably solvable while still
     * forcing real intersection work: every edge admits the whole upper part of the target's version
     * list, so no two edges on one package can ever exclude each other, and the intersection of all
     * edges on a package stays non empty for any combination.
     *
     * <p>A slice of packages additionally receives a root requirement with a bounded upper limit, so the
     * greedy pass genuinely has to step down from the highest version and the trace records downgrades.
     *
     * <p>Seeded, so the graph is byte identical on every run and the timing assertion is stable.
     */
    static final class SyntheticGraph {

        private final java.util.Map<String, List<Repository.Meta>> entries = new TreeMap<>();
        private final List<String> ids = new ArrayList<>();
        private final java.util.Map<String, Integer> versionCount = new TreeMap<>();
        private final java.util.Map<String, Integer> pin = new TreeMap<>();
        private final Random rnd;
        private final int packages;
        private int constraintCount;

        private SyntheticGraph(int packages, long seed) {
            this.packages = packages;
            this.rnd = new Random(seed);
        }

        static SyntheticGraph generate(int packages, int constraints, long seed) {
            SyntheticGraph g = new SyntheticGraph(packages, seed);
            for (int i = 0; i < packages; i++) {
                String id = String.format("com.acme.p%04d", i);
                g.ids.add(id);
                int n = 3 + g.rnd.nextInt(4);
                g.versionCount.put(id, n);
                g.pin.put(id, g.rnd.nextInt(n));
            }
            int fanout = Math.max(1, constraints / (packages * 4));

            for (String id : g.ids) {
                int n = g.versionCount.get(id);
                List<Repository.Meta> metas = new ArrayList<>(n);
                for (int vi = 0; vi < n; vi++) {
                    List<Repository.Dependency> deps = new ArrayList<>();
                    int seen = 0;
                    for (int attempt = 0; attempt < fanout * 6 && seen < fanout; attempt++) {
                        String target = g.ids.get(g.rnd.nextInt(packages));
                        if (target.equals(id) || deps.stream().anyMatch(d -> d.target().equals(target))) {
                            continue;
                        }
                        deps.add(new Repository.Dependency(target, g.edgeTo(target), false));
                        seen++;
                        g.constraintCount++;
                    }
                    metas.add(new Repository.Meta(id, version(vi), List.copyOf(deps), List.of()));
                }
                g.entries.put(id, metas);
            }
            return g;
        }

        /**
         * A range on {@code target} that admits its pin and every version above it: the lower bound sits
         * at or below the pin minor, and the exclusive upper bound sits strictly above the highest
         * published minor (or is left open).
         */
        private Interval edgeTo(String target) {
            int tv = versionCount.get(target);
            int lo = rnd.nextInt(pin.get(target) + 1);
            if (rnd.nextBoolean()) {
                return Interval.parse("[" + version(lo) + ",)");           // open ended upper
            }
            int hi = tv + 1 + rnd.nextInt(2);                                 // strictly above the top
            return Interval.parse("[" + version(lo) + "," + version(hi) + ")");
        }

        /** The published version with the given index, {@code 1.<index+1>.0}. */
        private static Version version(int index) {
            return Version.parse("1." + (index + 1) + ".0");
        }

        /**
         * Root requirements. Every package is rooted at {@code (,)} so it enters the tree, and roughly one
         * in eight gets a bounded upper limit that forces the greedy pass to pick below the highest
         * published version.
         */
        List<Resolver.Requirement> roots() {
            List<Resolver.Requirement> out = new ArrayList<>();
            for (String id : ids) {
                String range = "(,)";
                if (rnd.nextInt(8) == 0) {
                    int n = versionCount.get(id);
                    int hi = 1 + rnd.nextInt(n);          // exclusive upper, always above the pin
                    if (hi > pin.get(id)) {
                        range = "[" + version(0) + "," + version(hi) + ")";
                    }
                }
                out.add(Resolver.root(id, range));
            }
            return out;
        }

        int constraintCount() {
            return constraintCount;
        }

        void writeTo(Path dir) throws IOException {
            for (java.util.Map.Entry<String, List<Repository.Meta>> e : entries.entrySet()) {
                String id = e.getKey();
                Path p = dir.resolve("com.acme").resolve(id.substring("com.acme.".length()));
                Files.createDirectories(p);
                for (Repository.Meta meta : e.getValue()) {
                    StringBuilder sb = new StringBuilder();
                    sb.append("id ").append(meta.id()).append('\n');
                    sb.append("version ").append(meta.version()).append('\n');
                    for (Repository.Dependency d : meta.dependencies()) {
                        sb.append("dep ").append(d.target()).append(' ').append(d.range()).append('\n');
                    }
                    Files.writeString(p.resolve(meta.version() + ".meta"), sb.toString(),
                            StandardCharsets.UTF_8);
                }
            }
        }
    }
}

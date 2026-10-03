package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Iterative deepening and backtracking. These cases need a solution that is only reachable by stepping a
 * package down from the version greedy would pick, so they fail loudly if the repair window stops working.
 */
class BacktrackTest {

    private static Repository.Meta meta(String id, String version, String... deps) {
        List<Repository.Dependency> ds = new ArrayList<>();
        for (String spec : deps) {
            int sp = spec.lastIndexOf(' ');
            ds.add(new Repository.Dependency(spec.substring(0, sp),
                    Interval.parse(spec.substring(sp + 1)), false));
        }
        return new Repository.Meta(id, Version.parse(version), ds, List.of());
    }

    private static Repository repo(Repository.Meta... metas) {
        TreeMap<String, List<Repository.Meta>> m = new TreeMap<>();
        for (Repository.Meta meta : metas) {
            m.computeIfAbsent(meta.id(), k -> new ArrayList<>()).add(meta);
        }
        return Repository.of(m);
    }

    @Test
    @DisplayName("a solution reachable only by downgrading the greedy pick is found")
    void repairFindsLowerVersion() throws Exception {
        // Greedy takes app 1.1.0, which needs core [1.0,1.2) and so clashes with the root's core >=1.4.
        // The only assignment is app 1.0.0 with core 1.4.0.
        Repository r = repo(
                meta("a:app", "1.0.0", "a:core [1.4,1.6)"),
                meta("a:app", "1.1.0", "a:core [1.0,1.2)"),
                meta("a:core", "1.0.0"),
                meta("a:core", "1.4.0"),
                meta("a:core", "1.9.0"));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(
                Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,2.0)")));
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:app"),
                "app must be stepped down from the greedy 1.1.0");
        assertEquals(Version.parse("1.4.0"), res.selected().get("a:core"));
    }

    @Test
    @DisplayName("repair prefers the smallest change: a one step downgrade beats a distant one")
    void repairPrefersSmallestChange() throws Exception {
        // Two ways to satisfy core >=1.4: downgrade app one step, or downgrade a distant helper.
        // The window is tried smallest first, so the one step change must win.
        Repository r = repo(
                meta("a:app", "1.0.0", "a:core [1.4,1.6)", "a:helper [1.4,2.0)"),
                meta("a:app", "1.1.0", "a:core [1.0,1.2)", "a:helper [1.4,2.0)"),
                meta("a:core", "1.0.0"),
                meta("a:core", "1.4.0"),
                meta("a:helper", "1.0.0"),
                meta("a:helper", "1.4.0"));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(
                Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,2.0)")));
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:app"));
        assertEquals(Version.parse("1.4.0"), res.selected().get("a:core"));
        assertEquals(Version.parse("1.4.0"), res.selected().get("a:helper"));
    }

    @Test
    @DisplayName("an unsatisfiable requirement reports a conflict rather than looping")
    void unsatisfiableTerminates() {
        Repository r = repo(meta("a:app", "1.0.0", "a:core [1.0,1.2)"),
                meta("a:core", "1.0.0"), meta("a:core", "2.0.0"));
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class, () -> new Resolver(r).resolve(
                List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.5,3.0)"))));
        assertEquals("a:core", c.packageId());
        assertTrue(c.reason().contains("[1.0,1.2)"), c.reason());
        assertTrue(c.reason().contains("[1.5,3.0)"), c.reason());
    }

    @Test
    @DisplayName("the conflict names the two ranges, not just the package")
    void conflictNamesBothRanges() {
        Repository r = repo(meta("a:app", "1.0.0", "a:core [1.0,1.5)"),
                meta("a:core", "1.0.0"), meta("a:core", "2.0.0"));
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class, () -> new Resolver(r).resolve(
                List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.8,3.0)"))));
        assertTrue(c.reason().contains("mutually exclusive"), c.reason());
        assertTrue(c.reason().contains("[1.8,3.0)"), c.reason());
        assertTrue(c.reason().contains("[1.0,1.5)"), c.reason());
        // The chain must identify which site introduced the offending range.
        assertTrue(c.chain().stream().anyMatch(s -> s.contains("a:app -> a:core")),
                "chain should name the introducing site: " + c.chain());
    }

    @Test
    @DisplayName("a conflict whose ranges only exclude each other transitively still names a real pair")
    void transitiveExclusionIsReported() {
        // b is constrained to two ranges by two different packages; no version satisfies both.
        Repository r = repo(
                meta("a:p1", "1.0.0", "a:b [1.0,1.5)"),
                meta("a:p2", "1.0.0", "a:b [1.5,2.0)"),
                meta("a:b", "1.2.0"));
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class,
                () -> new Resolver(r).resolve(List.of(Resolver.root("a:p1", "(,)"),
                        Resolver.root("a:p2", "(,)"))));
        assertEquals("a:b", c.packageId());
        assertTrue(c.reason().contains("[1.0,1.5)"), c.reason());
        assertTrue(c.reason().contains("[1.5,2.0)"), c.reason());
    }

    @Test
    @DisplayName("a deep unsatisfiable chain terminates instead of recursing without bound")
    void deepUnsatisfiableTerminates() {
        List<Repository.Meta> metas = new ArrayList<>();
        for (int i = 0; i < 30; i++) {
            metas.add(meta("a:p" + i, "1.0.0", "a:p" + (i + 1) + " [1.0,1.5)"));
        }
        metas.add(meta("a:p30", "1.0.0"));
        Repository r = repo(metas.toArray(new Repository.Meta[0]));
        // The tail package cannot satisfy the narrowing range; the resolver must report, not hang.
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class, () -> new Resolver(r).resolve(
                List.of(Resolver.root("a:p0", "(,)"), Resolver.root("a:p30", "[2.0,3.0)"))));
        assertFalse(c.chain().isEmpty());
    }

    @Test
    @DisplayName("a cycle among packages resolves without looping")
    void cycleResolves() throws Exception {
        Repository r = repo(
                meta("a:x", "1.0.0", "a:y [1.0,2.0)"),
                meta("a:y", "1.0.0", "a:x [1.0,2.0)"),
                meta("a:x", "2.0.0", "a:y [1.0,2.0)"),
                meta("a:y", "2.0.0", "a:x [1.0,2.0)"));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(Resolver.root("a:x", "(,)")));
        assertEquals(2, res.size());
        // Greedy would take x=2.0.0, but y=1.0.0 then needs x in [1.0,2.0), which excludes 2.0.0, so the
        // only self consistent pair is 1.0.0 / 1.0.0. The cycle must be detected rather than re-placed.
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:x"));
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:y"));
    }

    @Test
    @DisplayName("the backtrack window is exactly 1..8 and the cap constant is 8")
    void windowCapIsEight() {
        assertEquals(8, Resolver.MAX_BACKTRACK_DEPTH);
        assertEquals(8, Resolver.MAX_FREE_BACKTRACK);
        assertTrue(Resolver.MAX_STEPS > 0, "a step budget must exist so the search always terminates");
    }

    @Test
    @DisplayName("resolution is identical across 20 runs, so backtracking does not leak state")
    void backtrackingIsDeterministic() throws Exception {
        Repository r = repo(
                meta("a:app", "1.0.0", "a:core [1.4,1.6)"),
                meta("a:app", "1.1.0", "a:core [1.0,1.2)"),
                meta("a:core", "1.0.0"),
                meta("a:core", "1.4.0"),
                meta("a:core", "1.9.0"));
        List<Resolver.Requirement> roots = List.of(
                Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,2.0)"));
        String expected = new Resolver(r).resolve(roots).selected().toString();
        for (int i = 0; i < 20; i++) {
            assertEquals(expected, new Resolver(r).resolve(roots).selected().toString(),
                    "run " + i + " diverged, backtracking leaked state between runs");
        }
    }

    @Test
    @DisplayName("the trace records the downgrade that repair performed")
    void traceRecordsRepairDowngrade() throws Exception {
        Repository r = repo(
                meta("a:app", "1.0.0", "a:core [1.4,1.6)"),
                meta("a:app", "1.1.0", "a:core [1.0,1.2)"),
                meta("a:core", "1.0.0"),
                meta("a:core", "1.4.0"),
                meta("a:core", "1.9.0"));
        Trace t = Trace.enabled();
        new Resolver(r, t).resolve(List.of(
                Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,2.0)")));
        String log = t.render();
        assertTrue(log.contains("a:app"), log);
        assertTrue(log.contains("1.0.0"), log);
        assertTrue(log.contains("conflict"), "the trace should note the conflict it recovered from: " + log);
    }

    @Test
    @DisplayName("a package that only appears transitively still enters the tree")
    void transitivePackageIncluded() throws Exception {
        Repository r = repo(meta("a:app", "1.0.0", "a:mid [1.0,2.0)"),
                meta("a:mid", "1.0.0", "a:leaf [1.0,2.0)"),
                meta("a:leaf", "1.3.0"));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(Resolver.root("a:app", "(,)")));
        assertEquals(3, res.size());
        assertEquals(Version.parse("1.3.0"), res.selected().get("a:leaf"));
        assertEquals(List.of("a:mid"), res.requiredBy().get("a:leaf"),
                "a:leaf should record a:mid as the constraining site");
    }
}

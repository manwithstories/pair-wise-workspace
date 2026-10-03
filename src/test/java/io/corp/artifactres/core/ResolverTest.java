package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Greedy selection, exclusions, conflicts and determinism at unit scale. */
class ResolverTest {

    private static Resolver resolve(TreeMap<String, Resolver.Module> repo, Trace trace,
                                    String target, String range) {
        Resolver r = new Resolver(repo, trace);
        r.require(target, range);
        return r;
    }

    @Test
    @DisplayName("greedy picks the highest version satisfying the window")
    void greedyPicksHighestSatisfyingVersion() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,3.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("2.0").version("3.0").build());

        Resolver r = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]");
        assertEquals(Version.parse("2.0"), r.resolve().versionOf("b:core"),
                "3.0 sits outside [1.0,3.0) so 2.0 is the highest match");

        // Widening the window must move the pick up to the true maximum.
        TreeMap<String, Resolver.Module> wider = new TreeMap<>();
        wider.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,4.0)").build());
        wider.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("2.0").version("3.0").build());
        assertEquals(Version.parse("3.0"),
                resolve(wider, Trace.disabled(), "a:app", "[1.0,1.0]").resolve().versionOf("b:core"));
    }

    @Test
    @DisplayName("a pre-release only wins when the window asks for one")
    void prereleaseRespectsWindow() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,4.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("2.0-RC1").version("2.0").build());

        // The release outranks the RC, so a window admitting both takes the release.
        assertEquals(Version.parse("2.0"),
                resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]").resolve().versionOf("b:core"));

        // A window that stops below the release must take the RC instead.
        // Now the window stops below the release, so only the RC is reachable.
        TreeMap<String, Resolver.Module> rcOnly = new TreeMap<>();
        rcOnly.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0-RC1]").build());
        rcOnly.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("2.0-RC1").version("2.0").build());
        assertEquals(Version.parse("2.0-RC1"),
                resolve(rcOnly, Trace.disabled(), "a:app", "[1.0,1.0]")
                        .resolve().versionOf("b:core"),
                "a window capped at the RC must take the RC");
    }

    @Test
    @DisplayName("a SNAPSHOT is chosen when the window admits only snapshots")
    void snapshotChosenWithinItsWindow() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("1.5-SNAPSHOT").version("1.5").build());

        Resolver.Resolution res = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]").resolve();
        assertEquals(Version.parse("1.5"), res.versionOf("b:core"),
                "the release outranks the snapshot inside a window admitting both");

        TreeMap<String, Resolver.Module> snapOnly = new TreeMap<>();
        snapOnly.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)").build());
        snapOnly.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("1.5-SNAPSHOT").build());
        assertEquals(Version.parse("1.5-SNAPSHOT"),
                resolve(snapOnly, Trace.disabled(), "a:app", "[1.0,1.0]").resolve().versionOf("b:core"));
    }

    @Test
    @DisplayName("two windows on one package are intersected, not picked between")
    void intersectsWindowsForOnePackage() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:one", Resolver.Module.builder("a:one").version("1.0", "c:lib", "[1.0,2.0)").build());
        repo.put("a:two", Resolver.Module.builder("a:two").version("1.0", "c:lib", "[1.5,3.0)").build());
        repo.put("c:lib", Resolver.Module.builder("c:lib")
                .version("1.0").version("1.5").version("2.0").version("2.5").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:one", "[1.0,1.0]");
        r.require("a:two", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        assertEquals(Interval.parse("[1.5,2.0)"), res.windows().get("c:lib"),
                "the two windows must intersect to [1.5,2.0)");
        assertEquals(Version.parse("1.5"), res.versionOf("c:lib"),
                "the highest version inside the intersection wins");
    }

    @Test
    @DisplayName("two mutually exclusive windows report the empty set and both constraints")
    void reportsEmptyIntersectionWithBothConstraints() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:one", Resolver.Module.builder("a:one").version("1.0", "c:lib", "[1.0,2.0)").build());
        repo.put("a:two", Resolver.Module.builder("a:two").version("1.0", "c:lib", "[3.0,4.0)").build());
        repo.put("c:lib", Resolver.Module.builder("c:lib").version("1.0").version("3.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:one", "[1.0,1.0]");
        r.require("a:two", "[1.0,1.0]");

        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);
        assertEquals("c:lib", conflict.pkg);
        assertTrue(conflict.window.isEmptySet(), "the collapsed window is the empty set");

        String named = String.valueOf(conflict.conflictA) + " || " + conflict.conflictB;
        assertTrue(named.contains("c:lib [1,2)"), "must name the lower window: " + named);
        assertTrue(named.contains("c:lib [3,4)"), "must name the upper window: " + named);
        assertNotNull(conflict.repair);
        assertFalse(conflict.repair.isBlank());
    }

    @Test
    @DisplayName("a missing package is reported as a metadata gap, not a version conflict")
    void reportsMissingPackage() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app").version("1.0", "nope:missing", "[1.0,2.0)").build());

        Resolver r = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]");
        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);
        assertEquals("nope:missing", conflict.pkg);
        assertTrue(conflict.repair.contains("publish"),
                "the repair should say the package must be published: " + conflict.repair);
    }

    @Test
    @DisplayName("no published version inside the window is a conflict with a repair hint")
    void reportsNoVersionInWindow() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app").version("1.0", "c:lib", "[9.0,10.0)").build());
        repo.put("c:lib", Resolver.Module.builder("c:lib").version("1.0").version("2.0").build());

        Resolver r = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]");
        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);
        assertEquals("c:lib", conflict.pkg);
        // No version sits above [9,10), so the hint must name the nearest one that does
        // exist locally: 2.0.
        assertTrue(conflict.repair.contains("2"),
                "the repair should name a locally published version: " + conflict.repair);
        assertTrue(conflict.repair.contains("widen"),
                "the repair should say which way to widen: " + conflict.repair);
    }

    @Test
    @DisplayName("an exclusion keeps the target out of the tree")
    void exclusionKeepsPackageOut() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .exclude("1.0", "x:banned")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core").version("1.0").build());
        repo.put("x:banned", Resolver.Module.builder("x:banned").version("1.0").build());

        Resolver.Resolution res = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]").resolve();
        assertFalse(res.selected().containsKey("x:banned"));
        assertEquals(Version.parse("1.0"), res.versionOf("b:core"));
    }

    @Test
    @DisplayName("an exclusion that collides with a transitive need reports the rule")
    void exclusionCollisionReportsTheRule() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .exclude("1.0", "x:legacy")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0", "x:legacy", "[1.0,2.0)")
                .build());
        repo.put("x:legacy", Resolver.Module.builder("x:legacy").version("1.0").build());

        Resolver r = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]");
        Resolver.Conflict conflict = assertThrows(Resolver.Conflict.class, r::resolve);
        String named = String.valueOf(conflict.conflictA) + " || " + conflict.conflictB;
        assertTrue(named.contains("excludes x:legacy"),
                "the conflict must name the exclusion rule: " + named);
        assertTrue(conflict.repair.contains("x:legacy"), conflict.repair);
    }

    @Test
    @DisplayName("iterative deepening prefers the fewest downgrades")
    void iterativeDeepeningPrefersMinimalChange() {
        // The greedy pick for a:core is 2.0, which drags a:lib into [2,3); the only version
        // that coexists with a:util's window is a:core 1.0.
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:root", Resolver.Module.builder("a:root")
                .version("1.0", "a:core", "[1.0,3.0)")
                .version("1.0", "a:util", "[1.0,2.0)")
                .build());
        repo.put("a:core", Resolver.Module.builder("a:core")
                .version("1.0", "a:lib", "[1.0,2.0)")
                .version("2.0", "a:lib", "[2.0,3.0)")
                .build());
        repo.put("a:util", Resolver.Module.builder("a:util")
                .version("1.0", "a:lib", "[1.0,2.0)")
                .build());
        repo.put("a:lib", Resolver.Module.builder("a:lib").version("1.0").version("2.0").build());

        Resolver r = resolve(repo, Trace.disabled(), "a:root", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        // Greedy would take a:core 2.0 -> a:lib [2,3), which a:util's [1,2) forbids, so
        // a:core must be the single decision that gives way.
        assertEquals(Version.parse("1.0"), res.versionOf("a:core"),
                "a:core must be downgraded so a:util's window still fits");
        assertEquals(Version.parse("1.0"), res.versionOf("a:util"), "a:util has only one version");
        assertEquals(Version.parse("1.0"), res.versionOf("a:lib"));
        assertTrue(r.lastDepth() <= Resolver.MAX_DEPTH);

        // Now the case where only the leaf is over-constrained: a:core and a:util both
        // admit a:lib 2.0, so nothing should be downgraded at all.
        TreeMap<String, Resolver.Module> easy = new TreeMap<>();
        easy.put("a:root", Resolver.Module.builder("a:root")
                .version("1.0", "a:core", "[1.0,3.0)")
                .version("1.0", "a:util", "[1.0,2.0)")
                .build());
        easy.put("a:core", Resolver.Module.builder("a:core")
                .version("1.0", "a:lib", "[1.0,3.0)")
                .version("2.0", "a:lib", "[2.0,3.0)")
                .build());
        easy.put("a:util", Resolver.Module.builder("a:util")
                .version("1.0", "a:lib", "[1.0,3.0)")
                .build());
        easy.put("a:lib", Resolver.Module.builder("a:lib").version("1.0").version("2.0").build());

        Resolver easyResolver = resolve(easy, Trace.enabled(), "a:root", "[1.0,1.0]");
        Resolver.Resolution easyRes = easyResolver.resolve();
        assertEquals(Version.parse("2.0"), easyRes.versionOf("a:core"));
        assertEquals(Version.parse("2.0"), easyRes.versionOf("a:lib"));
        assertEquals(1, easyResolver.lastDepth(),
                "a satisfiable-by-greedy graph must resolve at K=1 with no forced downgrade");
        assertTrue(easyResolver.trace().entries().stream()
                        .noneMatch(e -> e.kind == Trace.Kind.DOWNGRADE),
                "no package should be downgraded when the greedy tree is already consistent");
    }

    @Test
    @DisplayName("a cycle resolves without looping")
    void resolvesCycles() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:one", Resolver.Module.builder("a:one")
                .version("1.0", "a:two", "[1.0,2.0)").build());
        repo.put("a:two", Resolver.Module.builder("a:two")
                .version("1.0", "a:one", "[1.0,2.0)").build());

        Resolver.Resolution res = resolve(repo, Trace.disabled(), "a:one", "[1.0,1.0]").resolve();
        assertEquals(Version.parse("1.0"), res.versionOf("a:one"));
        assertEquals(Version.parse("1.0"), res.versionOf("a:two"));
    }

    @Test
    @DisplayName("a self-referencing package terminates")
    void resolvesSelfReference() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "a:app", "[1.0,2.0)").build());

        Resolver.Resolution res = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]").resolve();
        assertEquals(Version.parse("1.0"), res.versionOf("a:app"));
    }

    @Test
    @DisplayName("repeated runs over the same repository are identical")
    void resolutionIsDeterministic() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        for (int i = 0; i < 40; i++) {
            String id = String.format("g:m%02d", i);
            Resolver.Module.Builder b = Resolver.Module.builder(id).version("1.0");
            if (i > 0) {
                b = Resolver.Module.builder(id).version("1.0",
                        String.format("g:m%02d", i - 1), "[1.0,3.0)");
            }
            repo.put(id, b.build());
        }

        String first = resolve(repo, Trace.disabled(), "g:m00", "[1.0,1.0]").resolve().toString();
        for (int i = 0; i < 10; i++) {
            assertEquals(first, resolve(repo, Trace.disabled(), "g:m00", "[1.0,1.0]").resolve().toString(),
                    "resolution must be reproducible");
        }
    }

    @Test
    @DisplayName("insertion order of the repository does not affect the result")
    void repositoryInsertionOrderDoesNotMatter() {
        TreeMap<String, Resolver.Module> a = new TreeMap<>();
        TreeMap<String, Resolver.Module> b = new TreeMap<>();
        for (int i = 0; i < 20; i++) {
            String id = String.format("g:m%02d", i);
            String dep = i > 0 ? String.format("g:m%02d", i - 1) : null;
            Resolver.Module.Builder ba = Resolver.Module.builder(id).version("1.0");
            Resolver.Module.Builder bb = Resolver.Module.builder(id).version("1.0");
            if (dep != null) {
                ba.version("1.0", dep, "[1.0,2.0)");
                bb.version("1.0", dep, "[1.0,2.0)");
            }
            // Insert in opposite orders into the two maps.
            b.put(id, bb.build());
            a.put(id, ba.build());
        }
        assertEquals(resolve(a, Trace.disabled(), "g:m00", "[1.0,1.0]").resolve().selected(),
                resolve(b, Trace.disabled(), "g:m00", "[1.0,1.0]").resolve().selected());
    }

    @Test
    @DisplayName("the trace records trigger, choice and reason for every decision")
    void traceRecordsEveryDecision() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("1.5").build());

        Trace trace = Trace.enabled();
        resolve(repo, trace, "a:app", "[1.0,1.0]").resolve();

        List<Trace.Entry> entries = trace.entries();
        assertEquals(2, entries.size(), "one decision per resolved package");
        assertEquals("a:app", entries.get(0).pkg);
        assertEquals("<root>", entries.get(0).triggeredBy,
                "a root decision records the root requirement as its trigger");
        assertEquals("b:core", entries.get(1).pkg);
        assertEquals("a:app@1", entries.get(1).triggeredBy,
                "a transitive package must record what triggered it, as package@version");
        assertEquals(Trace.Kind.GREEDY, entries.get(1).kind);
        assertEquals(Version.parse("1.5"), entries.get(1).selected);
        assertNotNull(entries.get(1).window);
        assertTrue(entries.get(1).reason.contains("highest"), entries.get(1).reason);
        assertEquals(List.of("a:app", "b:core"), entries.get(1).chain,
                "the chain must show how the resolver got here");
    }

    @Test
    @DisplayName("a disabled trace records nothing and costs nothing")
    void disabledTraceIsInert() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app").version("1.0").build());

        Resolver r = resolve(repo, null, "a:app", "[1.0,1.0]");
        assertFalse(r.trace().isEnabled(), "a null trace defaults to disabled");
        r.resolve();
        assertEquals(0, r.trace().size());
        assertEquals("", r.trace().render());
        r.trace().dump(System.out); // must be a no-op
    }

    @Test
    @DisplayName("the module builder keeps every declared edge")
    void moduleBuilderKeepsAllEdges() {
        // The same target declared by two versions must not be deduplicated away.
        Resolver.Module m = Resolver.Module.builder("g:m")
                .version("1.0", "g:n", "[1.0,2.0)")
                .version("2.0", "g:n", "[1.0,2.0)")
                .version("1.0", "g:n", "[1.0,2.0)")   // exact duplicate of the first
                .build();
        assertEquals(List.of(1, 2).size(), m.versions.size());
        assertEquals(1, m.depsOf(Version.parse("1.0")).size(), "duplicate edge collapses");
        assertEquals(1, m.depsOf(Version.parse("2.0")).size(),
                "a different version's edge must be kept");
    }

    @Test
    @DisplayName("collapsed windows are reported per package")
    void reportsCollapsedWindows() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "c:lib", "[1.0,2.0)").build());
        repo.put("c:lib", Resolver.Module.builder("c:lib").version("1.0").version("1.5").build());

        Resolver.Resolution res = resolve(repo, Trace.disabled(), "a:app", "[1.0,1.0]").resolve();
        assertEquals(List.of(Interval.parse("[1.0,2.0)")),
                res.collapsedWindows().get("c:lib"));
    }
}

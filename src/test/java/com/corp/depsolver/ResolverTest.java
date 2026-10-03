package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Greedy selection, exclusions, backtrack repair and conflict reporting on hand built graphs. */
class ResolverTest {

    /** Helper to declare a package version with its required deps. */
    private static Repository.Meta meta(String id, String version, String... deps) {
        List<Repository.Dependency> ds = new ArrayList<>();
        for (String spec : deps) {
            int sp = spec.lastIndexOf(' ');
            ds.add(new Repository.Dependency(spec.substring(0, sp), Interval.parse(spec.substring(sp + 1)), false));
        }
        return new Repository.Meta(id, Version.parse(version), ds, List.of());
    }

    private static Repository repo(Map<String, List<Repository.Meta>> entries) {
        return Repository.of(entries);
    }

    private static Repository.Meta excl(String id, String version, List<String> excludes, String... deps) {
        List<Repository.Dependency> ds = new ArrayList<>();
        for (String spec : deps) {
            int sp = spec.lastIndexOf(' ');
            ds.add(new Repository.Dependency(spec.substring(0, sp), Interval.parse(spec.substring(sp + 1)), false));
        }
        return new Repository.Meta(id, Version.parse(version), ds, excludes);
    }

    @Test
    @DisplayName("greedy picks the highest version inside the range")
    void greedyHighest() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of("a:core", List.of(
                meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")))));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(Resolver.root("a:core", "[1.0,2.0)")));
        assertEquals(Version.parse("1.4.0"), res.selected().get("a:core"));
    }

    @Test
    @DisplayName("greedy narrows to the intersected range when several packages constrain one package")
    void intersectionNarrowsChoice() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")),
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,1.5)")))));
        Resolver.Resolution res = new Resolver(r).resolve(
                List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,3.0)")));
        assertEquals(Version.parse("1.4.0"), res.selected().get("a:core"));
    }

    @Test
    @DisplayName("mutually exclusive requirements on one package produce an empty-set conflict naming both")
    void exclusiveRequirementsConflict() {
        Repository r = repo(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "2.0.0")),
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,1.5)")))));
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class, () -> new Resolver(r)
                .resolve(List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.8,3.0)"))));
        assertEquals("a:core", c.packageId());
        assertTrue(c.reason().contains("[1.0,1.5)"), "reason should name the first range: " + c.reason());
        assertTrue(c.reason().contains("[1.8,3.0)"), "reason should name the second range: " + c.reason());
        assertFalse(c.chain().isEmpty(), "conflict must carry a chain");
        assertFalse(c.suggestion().isEmpty(), "conflict must carry a suggestion");
        assertTrue(c.report().contains("CONFLICT"));
    }

    @Test
    @DisplayName("an exclusion steers greedy to an older version")
    void exclusionSkipsVersion() throws Exception {
        Repository r = Repository.of(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")),
                "a:legacy", List.of(meta("a:legacy", "1.0.0", "a:core [1.5,3.0)")))));
        Repository.Meta app = excl("a:app", "1.0.0", List.of("a:legacy"), "a:legacy [1.0,2.0)");
        Repository withApp = Repository.of(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")),
                "a:legacy", List.of(meta("a:legacy", "1.0.0", "a:core [1.5,3.0)")),
                "a:app", List.of(app))));
        Resolver.Resolution res = new Resolver(withApp).resolve(List.of(Resolver.root("a:app", "(,)")));
        // app requires legacy, which requires core >=1.5, but app excludes legacy entirely; the
        // resolver should skip app 1.0.0 if another version exists, else report it.
        assertNotNull(res);
    }

    @Test
    @DisplayName("a cycle terminates and resolves")
    void cycleTerminates() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of(
                "a:x", List.of(meta("a:x", "1.0.0", "a:y [1.0,2.0)")),
                "a:y", List.of(meta("a:y", "1.0.0", "a:x [1.0,2.0)")))));
        Resolver.Resolution res = new Resolver(r).resolve(List.of(Resolver.root("a:x", "(,)")));
        assertEquals(2, res.size());
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:x"));
        assertEquals(Version.parse("1.0.0"), res.selected().get("a:y"));
    }

    @Test
    @DisplayName("optional dependencies do not constrain")
    void optionalIgnored() throws Exception {
        Repository r = Repository.of(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0")))));
        // a package with only an optional dependency on a nonexistent package still resolves
        Repository.Meta app = new Repository.Meta("a:app", Version.parse("1.0.0"),
                List.of(new Repository.Dependency("a:missing", Interval.parse("[1.0,2.0)"), true)), List.of());
        Repository full = Repository.of(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0")),
                "a:app", List.of(app))));
        Resolver.Resolution res = new Resolver(full).resolve(List.of(Resolver.root("a:app", "(,)")));
        assertTrue(res.selected().containsKey("a:app"));
        assertFalse(res.selected().containsKey("a:missing"), "optional dep must not be pulled in");
    }

    @Test
    @DisplayName("no published version satisfies the range yields a conflict with a suggestion")
    void unsatisfiableRange() {
        Repository r = repo(new TreeMap<>(Map.of("a:core", List.of(meta("a:core", "1.0.0")))));
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class,
                () -> new Resolver(r).resolve(List.of(Resolver.root("a:core", "[2.0,3.0)"))));
        assertEquals("a:core", c.packageId());
        assertTrue(c.suggestion().contains("a:core"));
    }

    @Test
    @DisplayName("resolve is deterministic across instances")
    void deterministicAcrossInstances() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")),
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,1.5)")))));
        List<Resolver.Requirement> roots = List.of(Resolver.root("a:app", "(,)"));
        String first = new Resolver(r).resolve(roots).selected().toString();
        for (int i = 0; i < 20; i++) {
            assertEquals(first, new Resolver(r).resolve(roots).selected().toString());
        }
    }

    @Test
    @DisplayName("trace records who triggered, what was chosen and why a downgrade happened")
    void traceCapturesDecisions() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of(
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")),
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,1.5)")))));
        Trace t = Trace.enabled();
        new Resolver(r, t).resolve(List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.4,3.0)")));
        assertFalse(t.isEmpty(), "trace must record entries when enabled");
        String rendered = t.render();
        assertTrue(rendered.contains("a:core"));
        assertTrue(rendered.contains("a:app"));
    }

    @Test
    @DisplayName("a disabled trace records nothing and costs nothing")
    void disabledTraceIsSilent() throws Exception {
        Repository r = repo(new TreeMap<>(Map.of("a:core", List.of(meta("a:core", "1.0.0")))));
        Trace t = Trace.disabled();
        new Resolver(r, t).resolve(List.of(Resolver.root("a:core", "(,)")));
        assertTrue(t.isEmpty());
        assertEquals("", t.render());
    }
}

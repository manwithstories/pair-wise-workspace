package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Decision tracing: ordering, content and the cost of having it switched off. */
class TraceTest {

    @Test
    @DisplayName("a disabled trace is inert")
    void disabledTraceIsInert() {
        Trace t = Trace.disabled();
        assertFalse(t.isEnabled());
        t.record("a", Trace.Kind.GREEDY, Version.parse("1.0"), Interval.any(), 1, 0, null, null, "why");
        t.push("a");
        t.pop();
        assertEquals(0, t.size());
        assertEquals(List.of(), t.entries());
        assertEquals("", t.render());
    }

    @Test
    @DisplayName("entries carry step, package, kind, version, window and reason")
    void entriesCaptureDecisionContext() {
        Trace t = Trace.enabled();
        t.push("a:root");
        t.record("a:root", Trace.Kind.GREEDY, Version.parse("1.0"),
                Interval.parse("[1.0,2.0)"), 3, 1, "<root>",
                "r:root -> a:root [1.0,2.0)", "highest version satisfying [1.0,2.0)");
        t.pop();

        Trace.Entry e = t.entries().get(0);
        assertEquals(1, e.step);
        assertEquals("a:root", e.pkg);
        assertEquals(Trace.Kind.GREEDY, e.kind);
        assertEquals(Version.parse("1.0"), e.selected);
        assertEquals(Interval.parse("[1.0,2.0)"), e.window);
        assertEquals(3, e.available);
        assertEquals(1, e.depth);
        assertEquals("<root>", e.triggeredBy);
        assertTrue(e.reason.contains("highest"));
        assertEquals(List.of("a:root"), e.chain);
    }

    @Test
    @DisplayName("steps are monotonic and unique")
    void stepsAreMonotonic() {
        Trace t = Trace.enabled();
        for (int i = 0; i < 5; i++) {
            t.record("p" + i, Trace.Kind.GREEDY, Version.parse("1.0"), Interval.any(), 1, 0,
                    null, null, "reason " + i);
        }
        int expected = 1;
        for (Trace.Entry e : t.entries()) {
            assertEquals(expected++, e.step, "steps must increase by one");
        }
    }

    @Test
    @DisplayName("the decision chain reflects the path taken")
    void chainReflectsPath() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app").version("1.0", "b:mid", "[1.0,2.0)").build());
        repo.put("b:mid", Resolver.Module.builder("b:mid").version("1.0", "c:leaf", "[1.0,2.0)").build());
        repo.put("c:leaf", Resolver.Module.builder("c:leaf").version("1.0").build());

        Trace t = Trace.enabled();
        Resolver r = new Resolver(repo, t);
        r.require("a:app", "[1.0,1.0]");
        r.resolve();

        List<Trace.Entry> leaf = t.entriesFor("c:leaf");
        assertEquals(1, leaf.size());
        assertEquals(List.of("a:app", "b:mid", "c:leaf"), leaf.get(0).chain,
                "the chain must show root -> mid -> leaf");
    }

    @Test
    @DisplayName("a no-candidate entry explains why nothing was chosen")
    void noCandidateEntryExplainsItself() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app").version("1.0", "b:missing", "[1.0,2.0)").build());

        Trace t = Trace.enabled();
        Resolver r = new Resolver(repo, t);
        r.require("a:app", "[1.0,1.0]");
        try {
            r.resolve();
        } catch (Resolver.Conflict expected) {
            // the trace must still explain the failure
        }

        List<Trace.Entry> missing = t.entriesFor("b:missing");
        assertFalse(missing.isEmpty(), "the missing package must appear in the trace");
        assertEquals(Trace.Kind.NO_CANDIDATE, missing.get(0).kind);
        assertTrue(missing.get(0).reason.contains("metadata"),
                "the reason should name the metadata gap: " + missing.get(0).reason);
    }

    @Test
    @DisplayName("entriesFor filters without disturbing order")
    void filtersByPackage() {
        Trace t = Trace.enabled();
        for (String p : List.of("a", "b", "a", "c", "b")) {
            t.record(p, Trace.Kind.GREEDY, Version.parse("1.0"), Interval.any(), 1, 0, null, null, "r");
        }
        List<Trace.Entry> a = t.entriesFor("a");
        assertEquals(2, a.size());
        assertEquals(List.of("a", "a"), List.of(a.get(0).pkg, a.get(1).pkg));
        assertEquals(1, a.get(0).step, "filtered entries keep their original steps");
        assertEquals(3, a.get(1).step);
    }

    @Test
    @DisplayName("render and dump produce the same text")
    void renderAndDumpAgree() {
        Trace t = Trace.enabled();
        t.record("a:pkg", Trace.Kind.DOWNGRADE, Version.parse("1.0"), Interval.parse("[1,2)"),
                3, 2, "a:other", "src", "forced downgrade");
        String rendered = t.render();
        assertTrue(rendered.contains("a:pkg"));
        assertTrue(rendered.contains("DOWNGRADE"));
        assertTrue(rendered.contains("forced downgrade"));

        ByteArrayOutputStream buf = new ByteArrayOutputStream();
        t.dump(new PrintStream(buf, true, StandardCharsets.UTF_8));
        String dumped = buf.toString(StandardCharsets.UTF_8);
        assertTrue(dumped.contains("resolve trace"), "dump() should announce itself");
        assertTrue(dumped.contains(rendered.trim()),
                "dump() must contain every line render() produced");
        assertTrue(rendered.contains("a:pkg") && dumped.contains("a:pkg"));
    }

    @Test
    @DisplayName("tracing does not change the resolution it observes")
    void tracingDoesNotAffectResults() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:root", Resolver.Module.builder("a:root")
                .version("1.0", "a:mid", "[1.0,4.0)")
                .version("1.0", "a:util", "[1.0,2.0)").build());
        repo.put("a:mid", Resolver.Module.builder("a:mid")
                .version("2.0", "a:leaf", "[1.0,2.0)")
                .version("3.0", "a:leaf", "[9.0,10.0)").build());
        repo.put("a:util", Resolver.Module.builder("a:util")
                .version("1.0", "a:leaf", "[1.0,2.0)").build());
        repo.put("a:leaf", Resolver.Module.builder("a:leaf").version("1.0").version("9.0").build());

        Resolver quiet = new Resolver(repo, Trace.disabled());
        quiet.require("a:root", "[1.0,1.0]");
        var withoutTrace = quiet.resolve();

        Resolver loud = new Resolver(repo, Trace.enabled());
        loud.require("a:root", "[1.0,1.0]");
        var withTrace = loud.resolve();

        assertEquals(withoutTrace.selected(), withTrace.selected(),
                "enabling the trace must not change the resolution");
        assertNotEquals(0, loud.trace().size(), "the enabled trace must have recorded decisions");
    }
}

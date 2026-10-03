package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Interval parsing, membership, intersection, disjointness and collapse. */
class IntervalTest {

    @Test
    @DisplayName("Maven-style literals parse to the expected bounds")
    void parsesLiteralForms() {
        Interval halfOpen = Interval.parse("[1.2,1.5)");
        assertEquals(Version.parse("1.2"), halfOpen.lo());
        assertEquals(Version.parse("1.5"), halfOpen.hi());
        assertTrue(halfOpen.loInclusive());
        assertFalse(halfOpen.hiInclusive());

        Interval closed = Interval.parse("[1.2,1.5]");
        assertTrue(closed.hiInclusive());

        Interval lowerOpen = Interval.parse("(1.2,1.5]");
        assertFalse(lowerOpen.loInclusive());
        assertTrue(lowerOpen.hiInclusive());

        // Bare version and bracket-singleton are both exact points.
        assertEquals(Interval.parse("[1.2]"), Interval.parse("1.2"));
        assertEquals(Version.parse("1.2"), Interval.parse("1.2").lo());
        assertEquals(Version.parse("1.2"), Interval.parse("1.2").hi());
    }

    @Test
    @DisplayName("unbounded forms round-trip and report no bound")
    void parsesUnboundedForms() {
        Interval above = Interval.parse("[1.2,)");
        assertEquals(Version.parse("1.2"), above.lo());
        assertNull(above.hi(), "an absent upper bound reads as null");
        assertFalse(above.isUnbounded());

        Interval below = Interval.parse("(,1.5)");
        assertNull(below.lo());
        assertEquals(Version.parse("1.5"), below.hi());

        assertTrue(Interval.any().isUnbounded());
        assertNull(Interval.any().lo());
        assertNull(Interval.any().hi());
    }

    @Test
    @DisplayName("degenerate and inverted ranges are the empty set")
    void detectsEmptySets() {
        assertTrue(Interval.none().isEmptySet());
        assertTrue(Interval.parse("[1.5,1.2)").isEmptySet(), "lower above upper");
        assertTrue(Interval.parse("[1.2,1.2)").isEmptySet(), "half-open singleton excludes its point");
        assertTrue(Interval.parse("(1.2,1.2)").isEmptySet());
        assertTrue(Interval.parse("(1.2)").isEmptySet(), "open singleton excludes its own point");
        assertFalse(Interval.parse("[1.2]").isEmptySet(), "closed singleton is a real point");
        assertTrue(Interval.exact(Version.parse("1.2")).contains(Version.parse("1.2.0")));
    }

    @Test
    @DisplayName("membership respects open and closed edges")
    void testsMembership() {
        Interval halfOpen = Interval.parse("[1.2,1.5)");
        assertTrue(halfOpen.contains(Version.parse("1.2")), "lower edge is inclusive");
        assertTrue(halfOpen.contains(Version.parse("1.4")));
        assertFalse(halfOpen.contains(Version.parse("1.5")), "upper edge is exclusive");
        assertFalse(halfOpen.contains(Version.parse("1.1")));

        Interval lowerOpen = Interval.parse("(1.2,1.5]");
        assertFalse(lowerOpen.contains(Version.parse("1.2")));
        assertTrue(lowerOpen.contains(Version.parse("1.5")), "upper edge is inclusive");

        assertTrue(Interval.any().contains(Version.parse("0.1")));
        assertTrue(Interval.any().contains(Version.parse("999")));
        assertFalse(Interval.none().contains(Version.parse("1.0")));
    }

    @Test
    @DisplayName("intersection narrows to the overlap")
    void intersectsOverlappingIntervals() {
        Interval a = Interval.parse("[1.2,1.5)");
        Interval b = Interval.parse("[1.4,2.0)");
        assertEquals(Interval.parse("[1.4,1.5)"), a.intersect(b));
        assertEquals(Interval.parse("[1.4,1.5)"), b.intersect(a), "intersection is commutative");

        assertEquals(Interval.parse("[1.2,1.4]"), a.intersect(Interval.parse("[1.0,1.4]")));
        // A shared edge stays exclusive unless BOTH sides include it: [1.4,1.5) n
        // [1.0,1.5] must not admit 1.5.
        assertEquals(Interval.parse("[1.4,1.5)"),
                Interval.parse("[1.4,1.5)").intersect(Interval.parse("[1.0,1.5]")));
        assertEquals(Interval.parse("[1.4,1.5)"),
                Interval.parse("[1.0,1.5]").intersect(Interval.parse("[1.4,1.5)")));
        assertEquals(Interval.parse("[1.4,1.5]"),
                Interval.parse("[1.4,1.5]").intersect(Interval.parse("[1.0,1.5]")));
    }

    @Test
    @DisplayName("mutually exclusive intervals intersect to the empty set and name both")
    void emptyIntersectionNamesBothConstraints() {
        Interval low = Interval.parse("[1.0,2.0)", "r:root -> a:lib [1.0,2.0)");
        Interval high = Interval.parse("[3.0,4.0)", "r:root -> a:lib [3.0,4.0)");

        Interval clash = low.intersect(high);
        assertTrue(clash.isEmptySet(), "disjoint windows must collapse to the empty set");
        assertTrue(low.isDisjoint(high));
        assertTrue(high.isDisjoint(low));

        // The origin must name both constraints so a reader knows which pair to fix.
        String origin = clash.origin();
        assertTrue(origin.contains("[1.0,2.0)"), "origin should name the lower window: " + origin);
        assertTrue(origin.contains("[3.0,4.0)"), "origin should name the upper window: " + origin);

        // Touching-but-exclusive edges are also mutually exclusive: [1,2) vs [2,3).
        assertTrue(Interval.parse("[1,2)").isDisjoint(Interval.parse("[2,3)")),
                "a half-open upper edge excludes the meeting point");
        assertFalse(Interval.parse("[1,2]").isDisjoint(Interval.parse("[2,3)"))); // 2 is covered
    }

    @Test
    @DisplayName("collapsing folds touching intervals into one, in stable order")
    void collapsesTouchingIntervals() {
        List<Interval> merged = Interval.collapse(List.of(
                Interval.parse("[1.0,1.5)"),
                Interval.parse("[1.5,2.0)"),   // touches the first on an included edge
                Interval.parse("[3.0,4.0)")));
        assertEquals(2, merged.size(), "the two touching intervals must fold into one");
        assertEquals(Interval.parse("[1.0,2.0)"), merged.get(0));
        assertEquals(Interval.parse("[3.0,4.0)"), merged.get(1));
    }

    @Test
    @DisplayName("collapsing keeps intervals that leave a real gap")
    void keepsGappedIntervalsSeparate() {
        // [1,1.5) and (1.5,2] both exclude 1.5, so the union has a hole.
        List<Interval> split = Interval.collapse(List.of(
                Interval.parse("[1.0,1.5)"), Interval.parse("(1.5,2.0]")));
        assertEquals(2, split.size(), "a genuine gap must not be folded away");

        // Input order must not change the result.
        List<Interval> reversed = Interval.collapse(List.of(
                Interval.parse("(1.5,2.0]"), Interval.parse("[1.0,1.5)")));
        assertEquals(split, reversed, "collapse output must be order-independent");
    }

    @Test
    @DisplayName("collapse output is stably ordered regardless of input order")
    void collapseIsStablyOrdered() {
        List<Interval> shuffled = Interval.collapse(List.of(
                Interval.parse("[5.0,6.0)"),
                Interval.parse("[1.0,2.0)"),
                Interval.parse("[3.0,3.5)")));
        assertEquals(3, shuffled.size());
        assertEquals(Version.parse("1.0"), shuffled.get(0).lo());
        assertEquals(Version.parse("3.0"), shuffled.get(1).lo());
        assertEquals(Version.parse("5.0"), shuffled.get(2).lo());

        // Dropping empty intervals keeps them out of the result entirely.
        assertEquals(0, Interval.collapse(List.of(Interval.none(), Interval.none())).size());
    }

    @Test
    @DisplayName("union spans both inputs")
    void unionsIntervals() {
        assertEquals(Interval.parse("[1.0,2.0)"),
                Interval.union(Interval.parse("[1.0,1.5)"), Interval.parse("[1.2,2.0)")));
        assertEquals(Interval.parse("[1.0,4.0]"),
                Interval.union(Interval.parse("[1.0,2.0)"), Interval.parse("[3.0,4.0]")));
        assertEquals(Interval.parse("[1.0,1.5)"), Interval.union(Interval.none(),
                Interval.parse("[1.0,1.5)")), "the empty set is the union identity");
    }

    @Test
    @DisplayName("maxSatisfying picks the highest matching version")
    void findsHighestSatisfyingVersion() {
        List<Version> versions = List.of(
                Version.parse("1.0"), Version.parse("1.5"), Version.parse("2.0"));
        assertEquals(Version.parse("1.5"),
                Interval.parse("[1.0,2.0)").maxSatisfying(versions));
        assertEquals(Version.parse("2.0"),
                Interval.parse("[1.0,2.0]").maxSatisfying(versions));
        assertNull(Interval.parse("[9.0,10.0)").maxSatisfying(versions));
    }

    @Test
    @DisplayName("malformed literals are rejected")
    void rejectsMalformedLiterals() {
        assertThrows(IllegalArgumentException.class, () -> Interval.parse(null));
        assertThrows(IllegalArgumentException.class, () -> Interval.parse(""));
        assertThrows(IllegalArgumentException.class, () -> Interval.parse("[1.2,1.5"));
        assertThrows(IllegalArgumentException.class, () -> Interval.parse("[1,2,3)"));
        assertThrows(IllegalArgumentException.class, () -> Interval.parse("]1,2["));
    }

    @Test
    @DisplayName("equality ignores provenance but respects the bounds")
    void equalityIgnoresOrigin() {
        assertEquals(Interval.parse("[1.0,2.0)", "origin A"),
                Interval.parse("[1.0,2.0)", "origin B"),
                "provenance is reporting metadata, not interval identity");
        assertNotEquals(Interval.parse("[1.0,2.0)"), Interval.parse("[1.0,2.0]"));
        assertNotEquals(Interval.parse("[1.0,2.0)"), Interval.parse("[1.0,2.5)"));
        assertEquals(Interval.parse("[1.0,2.0)").hashCode(), Interval.parse("[1.0,2.0)").hashCode());
    }

    @Test
    @DisplayName("the canonical form round-trips through parse")
    void canonicalFormRoundTrips() {
        for (String literal : List.of("[1.2,1.5)", "[1.2,1.5]", "(1.2,1.5]", "[1.2,)", "(,1.5)", "[1.2]")) {
            Interval i = Interval.parse(literal);
            assertEquals(i, Interval.parse(i.toString()),
                    "canonical form of " + literal + " must parse back to the same interval");
        }
        assertEquals("<empty>", Interval.none().toString());
        assertEquals("[,)", Interval.any().toString());
    }
}

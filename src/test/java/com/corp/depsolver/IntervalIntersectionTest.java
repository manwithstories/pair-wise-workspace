package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Interval intersection: overlapping, nested, touching, mutually exclusive and empty set reporting. */
class IntervalIntersectionTest {

    @Test
    @DisplayName("overlapping half-open ranges intersect to the overlap")
    void overlap() {
        Interval a = Interval.parse("[1.2,1.5)");
        Interval b = Interval.parse("[1.3,2.0-SNAPSHOT)");
        assertEquals(Interval.parse("[1.3,1.5)"), a.intersect(b));
        assertEquals(a.intersect(b), b.intersect(a));
    }

    @Test
    @DisplayName("nested ranges yield the inner one, commutatively")
    void nested() {
        Interval outer = Interval.parse("[1.0,2.0)");
        Interval inner = Interval.parse("[1.4,1.6)");
        assertEquals(inner, outer.intersect(inner));
        assertEquals(inner, inner.intersect(outer));
    }

    @Test
    @DisplayName("touching half-open ranges are mutually exclusive (empty set)")
    void touchingExclusiveIsEmpty() {
        Interval a = Interval.parse("[1.0,1.2)");
        Interval b = Interval.parse("[1.2,2.0)");
        assertTrue(a.intersect(b).isEmpty());
        assertTrue(b.intersect(a).isEmpty());
    }

    @Test
    @DisplayName("a closed point between two open bounds is still empty")
    void closedTouchingIsEmpty() {
        assertTrue(Interval.parse("(,1.0)").intersect(Interval.parse("[1.0,)")).isEmpty());
        assertTrue(Interval.parse("[1.0,1.0]").intersect(Interval.parse("(1.0,)")).isEmpty());
    }

    @Test
    @DisplayName("an exclusive bound meeting an inclusive bound at the same version is empty")
    void mixedTouchingIsEmpty() {
        assertTrue(Interval.parse("[1.0,1.2)").intersect(Interval.parse("(1.2,2.0]")).isEmpty());
        assertTrue(Interval.parse("(1.2,2.0]").intersect(Interval.parse("[1.0,1.2)")).isEmpty());
    }

    @Test
    @DisplayName("unbounded sides intersect into the bounded one")
    void unbounded() {
        assertEquals(Interval.parse("[1.0,2.0)"),
                Interval.any().intersect(Interval.parse("[1.0,2.0)")));
        assertEquals(Interval.parse("[1.0,2.0)"),
                Interval.parse("(,)").intersect(Interval.parse("[1.0,2.0)")));
        assertEquals(Interval.parse("[1.0,)"),
                Interval.parse("[1.0,)").intersect(Interval.any()));
        assertTrue(Interval.any().intersect(Interval.any()).isAny());
    }

    @Test
    @DisplayName("empty intersection names the two conflicting ranges")
    void emptySetNamesBothRanges() {
        List<Interval> parts = Arrays.asList(
                Interval.parse("[1.0,1.5)"),
                Interval.parse("[1.4,2.0)"),
                Interval.parse("[1.8,2.5)"));
        int[] pair = new int[2];
        Interval result = Interval.intersectAll(parts, pair);
        assertTrue(result.isEmpty());
        assertEquals(1, pair[0], "the part folded in before the break");
        assertEquals(2, pair[1], "the part that broke it");
        // The accumulator up to pair[0] is still valid, so a report can name the two sides precisely.
        Interval acc = Interval.intersectAll(parts.subList(0, pair[0] + 1), null);
        assertEquals("[1.4,1.5)", acc.toString());
        assertTrue(acc.intersect(parts.get(pair[1])).isEmpty());
    }

    @Test
    @DisplayName("intersectAll reports the first conflicting pair, not merely an empty result")
    void intersectAllReportsPair() {
        List<Interval> parts = Arrays.asList(
                Interval.parse("[1.0,2.0)"),
                Interval.parse("[3.0,4.0)"),
                Interval.parse("[0.5,3.5)"));
        int[] pair = new int[2];
        assertTrue(Interval.intersectAll(parts, pair).isEmpty());
        assertEquals(0, pair[0]);
        assertEquals(1, pair[1]);
    }

    @Test
    @DisplayName("snapshot upper bounds exclude the snapshot itself when the lower bound is closed")
    void snapshotSemantics() {
        Interval iv = Interval.parse("[1.3,2.0-SNAPSHOT)");
        assertTrue(iv.contains(Version.parse("1.3")));
        assertTrue(iv.contains(Version.parse("1.9.9")));
        assertFalse(iv.contains(Version.parse("2.0-SNAPSHOT")));
        assertFalse(iv.contains(Version.parse("2.0")));
    }

    @Test
    @DisplayName("adjacent ranges fold into one stable, ordered range")
    void foldAdjacent() {
        List<Interval> parts = new ArrayList<>(Arrays.asList(
                Interval.parse("[1.5,2.0)"),
                Interval.parse("[1.0,1.5)"),
                Interval.parse("[2.0,2.5)")));
        List<Interval> folded = Interval.foldAdjacent(parts);
        // Every touching version is covered by the following interval, so all three fold into one.
        assertEquals(1, folded.size());
        assertEquals("[1.0,2.5)", folded.get(0).toString());

        // folding is order independent
        List<Interval> shuffled = new ArrayList<>(Arrays.asList(
                Interval.parse("[1.0,1.5)"),
                Interval.parse("[2.0,2.5)"),
                Interval.parse("[1.5,2.0)")));
        assertEquals(folded.toString(), Interval.foldAdjacent(shuffled).toString());
    }

    @Test
    @DisplayName("ranges touching on one excluded side do not fold")
    void foldKeepsExcludedTouch() {
        List<Interval> folded = Interval.foldAdjacent(new ArrayList<>(Arrays.asList(
                Interval.parse("[1.0,1.5)"),
                Interval.parse("(1.5,2.0)"))));
        assertEquals(2, folded.size());
    }

    @Test
    @DisplayName("bare version shorthand means exactly that version")
    void bareVersionShorthand() {
        Interval iv = Interval.parse("1.2.3");
        assertTrue(iv.contains(Version.parse("1.2.3")));
        assertFalse(iv.contains(Version.parse("1.2.2")));
        assertFalse(iv.contains(Version.parse("1.2.4")));
    }

    @Test
    @DisplayName("parse rejects malformed intervals and self empty ones")
    void malformedRejected() {
        for (String bad : new String[]{"", "[1.0", "1.0]", "[1.0,1.0)", "[a,b)", "[,)", "[1.0,1.0]x"}) {
            boolean threw;
            try {
                Interval.parse(bad);
                threw = false;
            } catch (IllegalArgumentException e) {
                threw = true;
            }
            assertTrue(threw, "expected rejection of '" + bad + "'");
        }
    }
}

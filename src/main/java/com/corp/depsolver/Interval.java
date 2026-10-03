package com.corp.depsolver;

import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;

/**
 * A version range expressed with {@link TreeMap} boundaries.
 *
 * <p>The map is keyed by the lower bound version, so boundaries are held in {@link Version} natural
 * order and can be walked directly, which is what the resolver does when scanning candidates from
 * highest to lowest. Each value is a {@link Span} recording the upper bound together with the
 * inclusivity of both sides, which is what makes a half open interval such as {@code [1.2,1.5)}
 * expressible without lossy encoding.
 *
 * <p>Accepted textual forms: {@code [a,b]}, {@code [a,b)}, {@code (a,b]}, {@code (a,b)}, {@code
 * (,b]}, {@code [a,)}, {@code (,b)}, {@code (,)} and the bare shorthand {@code 1.2} meaning exactly
 * that one version. Rendering is always canonical, so two equal intervals print identically.
 *
 * <p>Intersection takes, per side, the maximum lower bound and the minimum upper bound, then re-derives
 * each side's inclusivity from whichever input bound fixed it. That single rule covers every half open
 * combination, including the two cases that are easy to get wrong: {@code [1.0,1.2) & [1.2,2.0) = ∅}
 * (the touching bound is excluded on one side) and {@code (,1.0) & [1.0,) = ∅}.
 */
public final class Interval {

    /**
     * Synthetic lowest bound, used as the lower key of an unbounded-below interval. Every real version
     * compares strictly above it, because a real version can never have a negative numeric segment.
     */
    public static final Version MIN_BOUND = Version.sentinel(new int[]{Integer.MIN_VALUE}, null);

    /** Synthetic highest bound, used as the upper key of an unbounded-above interval. */
    public static final Version MAX_BOUND =
            Version.sentinel(new int[]{Integer.MAX_VALUE, Integer.MAX_VALUE, Integer.MAX_VALUE}, null);

    /** One contiguous run of versions, with the inclusivity of each end. */
    public static final class Span {
        private final Version upper;
        private final boolean lowerInclusive;
        private final boolean upperInclusive;

        Span(Version upper, boolean lowerInclusive, boolean upperInclusive) {
            this.upper = upper;
            this.lowerInclusive = lowerInclusive;
            this.upperInclusive = upperInclusive;
        }

        public Version lower(Interval owner) {
            return owner.spans.firstKey();
        }

        public Version upper() {
            return upper;
        }

        public boolean lowerInclusive() {
            return lowerInclusive;
        }

        public boolean upperInclusive() {
            return upperInclusive;
        }
    }

    /** Canonical empty interval: the "no solution" signal returned by {@link #intersect}. */
    public static final Interval EMPTY = new Interval(new TreeMap<>(), "EMPTY");

    private final TreeMap<Version, Span> spans;
    private final String raw;

    private Interval(TreeMap<Version, Span> spans, String raw) {
        this.spans = spans;
        this.raw = raw;
    }

    /** Build from a lower/upper pair with explicit inclusivity flags. */
    private static Interval of(Version lo, boolean loInc, Version hi, boolean hiInc, String raw) {
        TreeMap<Version, Span> m = new TreeMap<>();
        Interval iv = new Interval(m, raw);
        if (isEmptySpan(lo, loInc, hi, hiInc)) {
            return EMPTY;
        }
        m.put(lo, new Span(hi, loInc, hiInc));
        return iv;
    }

    /** A span admits nothing when the bounds cross, or when they meet while both sides exclude. */
    private static boolean isEmptySpan(Version lo, boolean loInc, Version hi, boolean hiInc) {
        int c = lo.compareTo(hi);
        return c > 0 || (c == 0 && !(loInc && hiInc));
    }

    /** The unbounded interval {@code (,)}: every version, rendered canonically. */
    public static Interval any() {
        return of(MIN_BOUND, false, MAX_BOUND, false, "(,)");
    }

    /** A single exact version, written {@code [1.2.3,1.2.3]}. */
    public static Interval exact(Version v) {
        return of(v, true, v, true, null);
    }

    /** The unbounded-below interval {@code (,v]}. */
    public static Interval upToIncluding(Version v) {
        return of(MIN_BOUND, false, v, true, null);
    }

    /** The unbounded-above interval {@code [v,)}. */
    public static Interval fromIncluding(Version v) {
        return of(v, true, MAX_BOUND, false, null);
    }

    /**
     * Parse a textual interval. Anything that denotes no version at all is rejected, so an empty
     * interval can only enter the system as the explicit {@link #EMPTY} constant or as an intersection
     * result, never as a parsed range.
     */
    public static Interval parse(String text) {
        if (text == null) {
            throw new IllegalArgumentException("interval must not be null");
        }
        String s = text.trim();
        if (s.isEmpty()) {
            throw new IllegalArgumentException("interval must not be empty");
        }
        char first = s.charAt(0);
        if (first != '(' && first != '[') {
            // bare version shorthand: exactly that version
            return exact(Version.parse(s));
        }
        char last = s.charAt(s.length() - 1);
        boolean lowerOpen = first == '(';
        boolean lowerClosed = first == '[';
        boolean upperOpen = last == ')';
        boolean upperClosed = last == ']';
        if ((!lowerOpen && !lowerClosed) || (!upperOpen && !upperClosed)) {
            throw new IllegalArgumentException("malformed interval '" + text + "'");
        }
        int comma = s.indexOf(',');
        if (comma < 0 || s.indexOf(',', comma + 1) >= 0) {
            throw new IllegalArgumentException("malformed interval '" + text + "'");
        }
        String lo = s.substring(1, comma).trim();
        String hi = s.substring(comma + 1, s.length() - 1).trim();
        if (lo.isEmpty() && hi.isEmpty() && s.equals("(,)")) {
            return any();
        }
        if (lo.isEmpty() && hi.isEmpty()) {
            throw new IllegalArgumentException("interval '" + text + "' has neither bound");
        }
        Version lv = lo.isEmpty() ? MIN_BOUND : Version.parse(lo);
        Version uv = hi.isEmpty() ? MAX_BOUND : Version.parse(hi);
        Interval iv = of(lv, lowerClosed, uv, upperClosed, s);
        if (iv.isEmpty()) {
            throw new IllegalArgumentException("interval '" + text + "' is empty by construction");
        }
        return iv;
    }

    /** True for the unbounded interval {@code (,)}. */
    public boolean isAny() {
        Map.Entry<Version, Span> e = single();
        return e != null && e.getKey().equals(MIN_BOUND) && e.getValue().upper().equals(MAX_BOUND);
    }

    /** True when no version at all satisfies this interval. */
    public boolean isEmpty() {
        return spans.isEmpty();
    }

    private Map.Entry<Version, Span> single() {
        return spans.isEmpty() ? null : spans.firstEntry();
    }

    /** The lower boundary version, or {@code null} for an empty interval. */
    public Version lowerBound() {
        Map.Entry<Version, Span> e = single();
        return e == null ? null : e.getKey();
    }

    /** The upper boundary version, or {@code null} for an empty interval. */
    public Version upperBound() {
        Map.Entry<Version, Span> e = single();
        return e == null ? null : e.getValue().upper();
    }

    /** Whether the lower boundary version is itself included. */
    public boolean isLowerInclusive() {
        Map.Entry<Version, Span> e = single();
        return e != null && e.getValue().lowerInclusive();
    }

    /** Whether the upper boundary version is itself included. */
    public boolean isUpperInclusive() {
        Map.Entry<Version, Span> e = single();
        return e != null && e.getValue().upperInclusive();
    }

    public boolean contains(Version v) {
        if (v == null) {
            return false;
        }
        for (Map.Entry<Version, Span> e : spans.entrySet()) {
            Span s = e.getValue();
            int lo = e.getKey().compareTo(v);
            int hi = s.upper().compareTo(v);
            boolean loOk = s.lowerInclusive() ? lo <= 0 : lo < 0;
            boolean hiOk = s.upperInclusive() ? hi >= 0 : hi > 0;
            if (loOk && hiOk) {
                return true;
            }
        }
        return false;
    }

    /** Direct boundary view, in {@link Version} natural order. */
    public TreeMap<Version, Span> boundaries() {
        return spans;
    }

    /** Highest version inside this interval, or {@code null} when the interval is empty. */
    public Version highest(List<Version> candidatesDescending) {
        for (Version v : candidatesDescending) {
            if (contains(v)) {
                return v;
            }
        }
        return null;
    }

    /**
     * Intersect with {@code other}. Returns {@link #EMPTY} when the result admits no version, so callers
     * can tell "empty" apart from "unbounded" without extra flags.
     */
    public Interval intersect(Interval other) {
        TreeMap<Version, Span> out = new TreeMap<>();
        for (Map.Entry<Version, Span> a : spans.entrySet()) {
            for (Map.Entry<Version, Span> b : other.spans.entrySet()) {
                Span sa = a.getValue();
                Span sb = b.getValue();
                Version lo = max(a.getKey(), b.getKey());
                Version hi = min(sa.upper(), sb.upper());
                // The tightest bound wins on each side. When both inputs sit on the very same boundary
                // version that side is included only if every input includes it, so an exclusive bound
                // on either side excludes the point for the intersection too.
                boolean loInc = a.getKey().equals(lo) && b.getKey().equals(lo)
                        ? sa.lowerInclusive() && sb.lowerInclusive()
                        : a.getKey().equals(lo) ? sa.lowerInclusive() : sb.lowerInclusive();
                boolean hiInc = sa.upper().equals(hi) && sb.upper().equals(hi)
                        ? sa.upperInclusive() && sb.upperInclusive()
                        : sa.upper().equals(hi) ? sa.upperInclusive() : sb.upperInclusive();
                if (!isEmptySpan(lo, loInc, hi, hiInc)) {
                    out.put(lo, new Span(hi, loInc, hiInc));
                }
            }
        }
        if (out.isEmpty()) {
            return EMPTY;
        }
        return new Interval(out, render(out));
    }

    private static Version max(Version a, Version b) {
        return a.compareTo(b) >= 0 ? a : b;
    }

    private static Version min(Version a, Version b) {
        return a.compareTo(b) <= 0 ? a : b;
    }

    /**
     * Intersect a list of intervals in order, reporting the first pair that turned the result empty so
     * the caller can name the two mutually exclusive constraints.
     *
     * <p>{@code firstPair}, when supplied, receives the two indices: the last part folded into the
     * accumulator before it broke, and the part that broke it. The accumulator up to the first index is
     * still a valid interval, so a caller that wants to print the conflict can show
     * {@code intersectAll(parts[0..firstPair[0]])} against {@code parts[firstPair[1]]}.
     */
    public static Interval intersectAll(List<Interval> parts, int[] firstPair) {
        Interval acc = any();
        for (int i = 0; i < parts.size(); i++) {
            Interval next = acc.intersect(parts.get(i));
            if (next.isEmpty()) {
                if (firstPair != null && firstPair.length >= 2) {
                    firstPair[0] = i - 1;
                    firstPair[1] = i;
                }
                return EMPTY;
            }
            acc = next;
        }
        return acc;
    }

    /**
     * Collapse adjacent intervals whose union has no gap, returning a stably ordered list.
     * {@code [1.0,1.5)} and {@code [1.5,2.0)} fold into {@code [1.0,2.0)}, while {@code [1.0,1.5)} and
     * {@code (1.5,2.0)} stay split because the touching point is excluded on one side.
     */
    public static List<Interval> foldAdjacent(List<Interval> parts) {
        List<Interval> sorted = new ArrayList<>();
        for (Interval iv : parts) {
            if (!iv.isEmpty()) {
                sorted.add(iv);
            }
        }
        sorted.sort(Interval::compareByLowerBound);

        List<Interval> out = new ArrayList<>();
        Interval cur = null;
        for (Interval iv : sorted) {
            if (cur == null) {
                cur = iv;
                continue;
            }
            if (touchesWithoutGap(cur, iv)) {
                cur = join(cur, iv);
            } else {
                out.add(cur);
                cur = iv;
            }
        }
        if (cur != null) {
            out.add(cur);
        }
        List<Interval> deduped = new ArrayList<>();
        Set<String> seen = new LinkedHashSet<>();
        for (Interval iv : out) {
            if (seen.add(iv.toString())) {
                deduped.add(iv);
            }
        }
        return deduped;
    }

    private static int compareByLowerBound(Interval a, Interval b) {
        Version la = a.lowerBound();
        Version lb = b.lowerBound();
        if (la == null && lb == null) {
            return 0;
        }
        if (la == null) {
            return -1;
        }
        if (lb == null) {
            return 1;
        }
        return la.compareTo(lb);
    }

    /**
     * Whether {@code next} starts exactly where {@code cur} ends with no version missing in between.
     * A gap, or a shared boundary that either side excludes, means they cannot be merged.
     */
    private static boolean touchesWithoutGap(Interval cur, Interval next) {
        Version curHi = cur.upperBound();
        Version nextLo = next.lowerBound();
        if (curHi == null || nextLo == null) {
            return true;   // an unbounded side always continues the run
        }
        int c = curHi.compareTo(nextLo);
        if (c > 0) {
            return true;   // overlapping
        }
        if (c < 0) {
            return false;  // real gap between the two runs
        }
        // They meet at exactly one version. The union is still contiguous when that version is covered
        // by either side, so [1.0,1.5) + [1.5,2.0) folds but [1.0,1.5) + (1.5,2.0) does not, since
        // 1.5 itself belongs to neither.
        return cur.isUpperInclusive() || next.isLowerInclusive();
    }

    /** The smallest interval covering both inputs. */
    private static Interval join(Interval a, Interval b) {
        Version lo = a.lowerBound().compareTo(b.lowerBound()) <= 0 ? a.lowerBound() : b.lowerBound();
        Version hi = a.upperBound().compareTo(b.upperBound()) >= 0 ? a.upperBound() : b.upperBound();
        boolean loInc = a.lowerBound().equals(lo) ? a.isLowerInclusive() : b.isLowerInclusive();
        boolean hiInc = a.upperBound().equals(hi) ? a.isUpperInclusive() : b.isUpperInclusive();
        return of(lo, loInc, hi, hiInc, null);
    }

    /** Canonical text for a boundary map. */
    private static String render(TreeMap<Version, Span> m) {
        if (m.isEmpty()) {
            return "EMPTY";
        }
        if (m.size() > 1) {
            return "UNION";
        }
        Map.Entry<Version, Span> e = m.firstEntry();
        Version lo = e.getKey();
        Span s = e.getValue();
        boolean loInf = lo.equals(MIN_BOUND);
        boolean hiInf = s.upper().equals(MAX_BOUND);
        String loS = loInf ? "" : lo.toString();
        String hiS = hiInf ? "" : s.upper().toString();
        return (loInf || s.lowerInclusive() ? "[" : "(")
                + loS + "," + hiS
                + (hiInf || !s.upperInclusive() ? ")" : "]");
    }

    /** The text this interval was parsed from, or its canonical form when built programmatically. */
    public String raw() {
        return raw != null ? raw : toString();
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof Interval && toString().equals(o.toString());
    }

    @Override
    public int hashCode() {
        return toString().hashCode();
    }

    @Override
    public String toString() {
        return spans.isEmpty() ? "EMPTY" : render(spans);
    }
}

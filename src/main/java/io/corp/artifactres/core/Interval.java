package io.corp.artifactres.core;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * Immutable half-open version interval expressed as a {@link TreeMap} of boundaries.
 *
 * <p>Invariant: for a non-empty, non-unbounded interval the map holds exactly two
 * entries, the first being the lower edge and the last the upper edge
 * (lower &le; upper, guaranteed by the constructor check in {@link #of}). A single
 * entry denotes the exact singleton {@code [v,v]}; an empty map denotes the unbounded
 * interval. Because the map is keyed by {@link Version}, iteration follows natural
 * version order, which is what makes {@link #collapse(List)} and the conflict report
 * deterministic across runs and JVMs.
 *
 * <p>Accepted syntax (Maven-style):
 * <pre>
 *   [1.2,1.5)   lower inclusive, upper exclusive   (default form)
 *   [1.2,1.5]   closed
 *   (1.2,1.5]   lower exclusive
 *   [1.2,)      unbounded above
 *   (,1.5)      unbounded below
 *   [1.2]       exact single version
 *   1.2         bare version, treated as [1.2,1.2]
 *   (1.2)       empty (excluded singleton)
 * </pre>
 */
public final class Interval {

    /** version -&gt; isInclusive. Sorted by natural version order. */
    private final TreeMap<Version, Boolean> bounds;
    /** Provenance label, used to name the two conflicting constraints in a report. */
    private final String origin;
    /** True when this interval denotes the empty set. */
    private final boolean empty;

    private Interval(TreeMap<Version, Boolean> bounds, String origin, boolean empty) {
        this.bounds = bounds;
        this.origin = origin;
        this.empty = empty;
    }

    // ---------------------------------------------------------------- factories

    /** The interval matching every version. Built directly: of() with no bounds is this. */
    public static Interval any() {
        TreeMap<Version, Boolean> m = new TreeMap<>();
        m.put(Version.MIN, Boolean.TRUE);
        m.put(Version.MAX, Boolean.FALSE);
        return new Interval(m, null, false);
    }

    /** The interval matching nothing. */
    public static Interval none() {
        return new Interval(new TreeMap<>(), null, true);
    }

    /** Exact single version, as {@code [v,v]}. */
    public static Interval exact(Version v) {
        TreeMap<Version, Boolean> m = new TreeMap<>();
        m.put(v, Boolean.TRUE);
        return new Interval(m, null, false);
    }

    /** @param lower {@code null} means unbounded below; likewise {@code upper}. */
    public static Interval of(Version lower, boolean lowerInclusive, Version upper, boolean upperInclusive) {
        if (lower == null && upper == null) {
            return any();
        }
        if (lower != null && upper != null) {
            int c = lower.compareTo(upper);
            if (c > 0) {
                return none();
            }
            if (c == 0 && !(lowerInclusive && upperInclusive)) {
                // Degenerate [v,v), (v,v], (v,v) all exclude the single point v.
                return none();
            }
        }
        TreeMap<Version, Boolean> m = new TreeMap<>();
        // Absent bounds become sentinels, so firstEntry() is always the lower edge
        // and lastEntry() the upper edge, and toString() round-trips the literal.
        m.put(lower == null ? Version.MIN : lower, lowerInclusive);
        m.put(upper == null ? Version.MAX : upper, upperInclusive);
        if (lower != null && upper != null && lower.compareTo(upper) == 0) {
            m.clear();
            m.put(lower, Boolean.TRUE); // exact singleton
        }
        return new Interval(m, null, false);
    }

    public static Interval parse(String text) {
        return parse(text, null);
    }

    /**
     * @param text   interval literal, see the class javadoc for accepted forms
     * @param origin provenance label recorded on the interval for conflict reporting
     */
    public static Interval parse(String text, String origin) {
        if (text == null) {
            throw new IllegalArgumentException("interval literal must not be null");
        }
        String s = text.trim();
        if (s.isEmpty()) {
            throw new IllegalArgumentException("interval literal must not be empty");
        }

        boolean lowerInclusive;
        char first = s.charAt(0);
        if (first == '[') {
            lowerInclusive = true;
        } else if (first == '(') {
            lowerInclusive = false;
        } else {
            if (first == ']' || first == ')') {
                throw new IllegalArgumentException("malformed interval: '" + text + "'");
            }
            // Bare version: exact point.
            return exact(Version.parse(s)).withOrigin(origin);
        }

        char last = s.charAt(s.length() - 1);
        if (last != ']' && last != ')') {
            throw new IllegalArgumentException("unterminated interval: '" + text + "'");
        }
        boolean upperInclusive = last == ']';
        String body = s.substring(1, s.length() - 1);

        if (body.indexOf(',') < 0) {
            String singleton = body.trim();
            if (singleton.isEmpty()) {
                throw new IllegalArgumentException("malformed interval: '" + text + "'");
            }
            Version v = Version.parse(singleton);
            // "[1.2]" is exact; "(1.2)" excludes its own endpoint and is therefore empty.
            Interval r = (lowerInclusive && upperInclusive) ? exact(v) : none();
            return r.withOrigin(origin);
        }

        int comma = body.indexOf(',');
        if (body.indexOf(',', comma + 1) >= 0) {
            throw new IllegalArgumentException("interval has more than two bounds: '" + text + "'");
        }
        String loText = body.substring(0, comma).trim();
        String hiText = body.substring(comma + 1).trim();
        Version lower = loText.isEmpty() ? null : Version.parse(loText);
        Version upper = hiText.isEmpty() ? null : Version.parse(hiText);

        Interval r = of(lower, lowerInclusive, upper, upperInclusive);
        return r.withOrigin(origin);
    }

    /** @return a copy of this interval carrying the given provenance label. */
    public Interval withOrigin(String newOrigin) {
        if (newOrigin == null || newOrigin.equals(origin)) {
            return this;
        }
        return new Interval(bounds, newOrigin, empty);
    }

    // ---------------------------------------------------------------- accessors

    /** @return the lower edge, or {@code null} when unbounded below. */
    public Version lo() {
        Map.Entry<Version, Boolean> e = bounds.firstEntry();
        if (e == null || e.getKey().isSentinel()) {
            return null;
        }
        return e.getKey();
    }

    /** @return the upper edge, or {@code null} when unbounded above. */
    public Version hi() {
        Map.Entry<Version, Boolean> e = bounds.lastEntry();
        if (e == null || e.getKey().isSentinel()) {
            return null;
        }
        return e.getKey();
    }

    /** @return true when a lower edge was recorded (always; sentinels included). */
    private boolean hasLo() {
        Map.Entry<Version, Boolean> e = bounds.firstEntry();
        return e != null && !e.getKey().isSentinel();
    }

    /** @return true when an upper edge was recorded. */
    private boolean hasHiEdge() {
        Map.Entry<Version, Boolean> e = bounds.lastEntry();
        return e != null && !e.getKey().isSentinel();
    }

    /** @return {@code true} when the lower edge is inclusive (true when unbounded). */
    public boolean loInclusive() {
        Map.Entry<Version, Boolean> e = bounds.firstEntry();
        return e == null || e.getValue();
    }

    /** @return {@code true} when the upper edge is inclusive (false when unbounded). */
    public boolean hiInclusive() {
        Map.Entry<Version, Boolean> e = bounds.lastEntry();
        return e != null && e.getValue();
    }

    /** @return {@code true} when this interval matches every version. */
    public boolean isUnbounded() {
        return !empty && !hasLo() && !hasHiEdge();
    }

    /** @return {@code true} when no version can satisfy this interval. */
    public boolean isEmptySet() {
        return empty;
    }

    public boolean isEmpty() {
        return empty;
    }

    /** @return a copy of the boundary map, sorted by natural version order. */
    public TreeMap<Version, Boolean> bounds() {
        return new TreeMap<>(bounds);
    }

    public String origin() {
        return origin;
    }

    /** @return {@code true} when {@code v} satisfies this interval. */
    public boolean contains(Version v) {
        if (empty) {
            return false;
        }
        Map.Entry<Version, Boolean> lo = bounds.firstEntry();
        Map.Entry<Version, Boolean> hi = bounds.lastEntry();
        if (lo != null && !lo.getKey().isSentinel()) {
            int c = v.compareTo(lo.getKey());
            if (c < 0 || (c == 0 && !lo.getValue())) {
                return false;
            }
        }
        if (hi != null && !hi.getKey().isSentinel()) {
            int c = v.compareTo(hi.getKey());
            if (c > 0 || (c == 0 && !hi.getValue())) {
                return false;
            }
        }
        return true;
    }

    /** @return the highest version in {@code candidates} satisfying this interval, or null. */
    public Version maxSatisfying(List<Version> candidates) {
        Version best = null;
        for (int i = 0; i < candidates.size(); i++) {
            Version v = candidates.get(i);
            if (contains(v) && (best == null || v.compareTo(best) > 0)) {
                best = v;
            }
        }
        return best;
    }

    // ------------------------------------------------------------------ algebra

    /**
     * Intersect this interval with {@code other}.
     *
     * @return the intersection; {@link #none()} when the two are mutually exclusive.
     *         The result's {@link #origin()} names both inputs, so an empty result can
     *         be reported with the exact pair of constraints that excluded each other.
     */
    public Interval intersect(Interval other) {
        if (empty || other.empty) {
            return noneWithOrigin(describePair(other));
        }
        Interval r = of(
                pickLo(other),
                loIncAgainst(other),
                pickHi(other),
                hiIncAgainst(other));
        if (r.isEmptySet()) {
            return noneWithOrigin(describePair(other));
        }
        return r.withOrigin(describePair(other));
    }

    private String describePair(Interval other) {
        String mine = origin == null ? toString() : origin;
        String theirs = other.origin == null ? other.toString() : other.origin;
        return mine + " \u2229 " + theirs;
    }

    private static Interval noneWithOrigin(String origin) {
        return new Interval(new TreeMap<Version, Boolean>(), origin, true);
    }

    /** Greater of the two lower edges (unbounded below loses to any real edge). */
    private Version pickLo(Interval other) {
        Version a = lo();
        Version b = other.lo();
        if (a == null) {
            return b;
        }
        if (b == null) {
            return a;
        }
        int c = a.compareTo(b);
        return c >= 0 ? a : b;
    }

    /** Inclusive only when the surviving edge is inclusive on the side that supplied it. */
    private boolean loIncAgainst(Interval other) {
        Version a = lo();
        Version b = other.lo();
        if (a == null) {
            return other.loInclusive();
        }
        if (b == null) {
            return loInclusive();
        }
        int c = a.compareTo(b);
        if (c != 0) {
            return c > 0 ? loInclusive() : other.loInclusive();
        }
        // Same edge value: inclusive only if both admit it.
        return loInclusive() && other.loInclusive();
    }

    /** Lesser of the two upper edges (unbounded above loses to any real edge). */
    private Version pickHi(Interval other) {
        Version a = hi();
        Version b = other.hi();
        if (a == null) {
            return b;
        }
        if (b == null) {
            return a;
        }
        int c = a.compareTo(b);
        return c <= 0 ? a : b;
    }

    private boolean hiIncAgainst(Interval other) {
        Version a = hi();
        Version b = other.hi();
        if (a == null) {
            return other.hiInclusive();
        }
        if (b == null) {
            return hiInclusive();
        }
        int c = a.compareTo(b);
        if (c != 0) {
            return c < 0 ? hiInclusive() : other.hiInclusive();
        }
        // Same upper edge: the intersection must not include it unless BOTH sides do.
        return hiInclusive() && other.hiInclusive();
    }

    /** @return {@code true} when the two intervals share no version at all. */
    public boolean isDisjoint(Interval other) {
        return intersect(other).isEmptySet();
    }

    /**
     * Merge intervals into a minimal, stable, ascending list.
     *
     * <p>Overlapping and touching intervals are folded into their span. Distinct
     * singleton points are never folded, since each remains satisfiable on its own.
     * The result is sorted by natural version order, so it is byte-stable.
     */
    public static List<Interval> collapse(List<Interval> input) {
        List<Interval> live = new ArrayList<>();
        for (Interval i : input) {
            if (i != null && !i.isEmptySet()) {
                live.add(i);
            }
        }
        if (live.isEmpty()) {
            return live;
        }
        live.sort(INTERVAL_ORDER);

        List<Interval> out = new ArrayList<>(live.size());
        Interval cur = live.get(0);
        for (int i = 1; i < live.size(); i++) {
            Interval next = live.get(i);
            if (overlapsOrTouches(cur, next)) {
                cur = union(cur, next);
            } else {
                out.add(cur);
                cur = next;
            }
        }
        out.add(cur);
        return out;
    }

    /** Ascending by lower edge (unbounded first), then upper edge (unbounded last). */
    private static final java.util.Comparator<Interval> INTERVAL_ORDER = (a, b) -> {
        Version al = a.lo();
        Version bl = b.lo();
        if (al == null || bl == null) {
            if (al == null && bl == null) {
                // fall through to the upper-edge comparison
            } else {
                return al == null ? -1 : 1;
            }
        } else {
            int c = al.compareTo(bl);
            if (c != 0) {
                return c;
            }
        }
        Version ah = a.hi();
        Version bh = b.hi();
        if (ah == null || bh == null) {
            if (ah == null && bh == null) {
                return 0;
            }
            return ah == null ? 1 : -1;
        }
        int c = ah.compareTo(bh);
        if (c != 0) {
            return c;
        }
        return Boolean.compare(a.loInclusive(), b.loInclusive());
    };

    /** @return true when {@code next} overlaps {@code cur} or begins exactly where it ends. */
    private static boolean overlapsOrTouches(Interval cur, Interval next) {
        Version curHi = cur.hi();
        Version nextLo = next.lo();
        if (curHi == null || nextLo == null) {
            return true;
        }
        int c = curHi.compareTo(nextLo);
        if (c > 0) {
            return true;
        }
        if (c == 0) {
            // They meet at exactly one point. The union has no gap unless *neither*
            // side admits that point, so fold whenever either one includes it.
            //   [1.0,1.5) + [1.5,2.0) -> [1.0,2.0)   (1.5 is in the second)
            //   [1.0,1.5] + (1.5,2.0] -> [1.0,2.0]   (1.5 is in the first)
            //   [1.0,1.5) + (1.5,2.0] -> stays split (1.5 belongs to neither)
            return cur.hiInclusive() || next.loInclusive();
        }
        return false;
    }

    /** @return the smallest interval containing both inputs. */
    public static Interval union(Interval a, Interval b) {
        if (a.isEmptySet()) {
            return b;
        }
        if (b.isEmptySet()) {
            return a;
        }
        Version lo;
        boolean loInc;
        if (a.lo() == null) {
            lo = b.lo();
            loInc = b.loInclusive();
        } else if (b.lo() == null) {
            lo = a.lo();
            loInc = a.loInclusive();
        } else {
            int c = a.lo().compareTo(b.lo());
            if (c < 0) {
                lo = a.lo();
                loInc = a.loInclusive();
            } else if (c > 0) {
                lo = b.lo();
                loInc = b.loInclusive();
            } else {
                lo = a.lo();
                loInc = a.loInclusive() && b.loInclusive();
            }
        }

        Version hi;
        boolean hiInc;
        if (a.hi() == null) {
            hi = b.hi();
            hiInc = b.hiInclusive();
        } else if (b.hi() == null) {
            hi = a.hi();
            hiInc = a.hiInclusive();
        } else {
            int c = a.hi().compareTo(b.hi());
            if (c > 0) {
                hi = a.hi();
                hiInc = a.hiInclusive();
            } else if (c < 0) {
                hi = b.hi();
                hiInc = b.hiInclusive();
            } else {
                hi = a.hi();
                hiInc = a.hiInclusive() && b.hiInclusive();
            }
        }
        return of(lo, loInc, hi, hiInc);
    }

    @Override
    public String toString() {
        if (empty) {
            return "<empty>";
        }
        StringBuilder sb = new StringBuilder(16);
        sb.append(loInclusive() ? '[' : '(');
        if (lo() != null) {
            sb.append(lo());
        }
        sb.append(',');
        if (hi() != null) {
            sb.append(hi());
        }
        sb.append(hiInclusive() ? ']' : ')');
        return sb.toString();
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof Interval)) {
            return false;
        }
        Interval i = (Interval) o;
        // Ignore origin: it is reporting metadata, not interval identity.
        return empty == i.empty && bounds.equals(i.bounds);
    }

    @Override
    public int hashCode() {
        return bounds.hashCode() * 31 + (empty ? 1 : 0);
    }
}

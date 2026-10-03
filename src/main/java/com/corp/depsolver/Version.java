package com.corp.depsolver;

import java.util.ArrayList;
import java.util.List;

/**
 * Semantic version value object: numeric segments plus an optional pre-release tail.
 *
 * <p>Grammar accepted (hand written, no third party parser):
 * <pre>
 *   version   := numeric ( '-' pre )?
 *   numeric   := segment ( '.' segment )*
 *   segment    := digit+
 *   pre       := identifier ( '.' identifier )*
 *   identifier := digit+ | [0-9A-Za-z-]+
 * </pre>
 *
 * <p>{@code SNAPSHOT} is just a pre-release identifier, so {@code 1.2.3-SNAPSHOT} sorts strictly
 * below {@code 1.2.3}, and {@code 2.0-SNAPSHOT} is a usable upper bound. Ordering follows SemVer 2.0.0
 * precedence: numeric segments compare numerically (zero padded, any length), a pre-release tail sorts
 * below the plain release, pre-release identifiers compare per-identifier (numeric identifiers
 * numerically and always below alphanumeric ones, alphanumeric lexically, shorter prefix below longer).
 *
 * <p>The natural order defined here is the single ordering used everywhere: the resolver keeps its
 * candidate versions in {@code TreeMap<Version, ...>} and walks them in natural order.
 */
public final class Version implements Comparable<Version> {

    /** Canonical version meaning "no tail", used for display only. */
    private static final String NO_PRE = "";

    private final int[] numeric;
    private final List<String> pre;
    private final String rawPre;
    private final String raw;

    private Version(int[] numeric, List<String> pre, String rawPre, String raw) {
        this.numeric = numeric;
        this.pre = pre;
        this.rawPre = rawPre;
        this.raw = raw;
    }

    public static Version parse(String text) {
        if (text == null) {
            throw new IllegalArgumentException("version must not be null");
        }
        String s = text.trim();
        if (s.isEmpty()) {
            throw new IllegalArgumentException("version must not be empty");
        }
        int dash = s.indexOf('-');
        String numericPart = dash < 0 ? s : s.substring(0, dash);
        String prePart = dash < 0 ? null : s.substring(dash + 1);

        if (numericPart.isEmpty()) {
            throw new IllegalArgumentException("missing numeric segment in version '" + text + "'");
        }
        String[] segs = numericPart.split("\\.", -1);
        int[] numeric = new int[segs.length];
        for (int i = 0; i < segs.length; i++) {
            String seg = segs[i];
            if (seg.isEmpty() || !isAllDigits(seg)) {
                throw new IllegalArgumentException(
                        "invalid numeric segment '" + seg + "' in version '" + text + "'");
            }
            if (seg.length() > 9) {
                throw new IllegalArgumentException("numeric segment too large in version '" + text + "'");
            }
            numeric[i] = Integer.parseInt(seg);
        }

        List<String> pre = List.of();
        if (prePart != null) {
            if (prePart.isEmpty()) {
                throw new IllegalArgumentException("empty pre-release in version '" + text + "'");
            }
            String[] ids = prePart.split("\\.", -1);
            List<String> tmp = new ArrayList<>(ids.length);
            for (String id : ids) {
                if (id.isEmpty() || !isIdentifier(id)) {
                    throw new IllegalArgumentException(
                            "invalid pre-release identifier '" + id + "' in version '" + text + "'");
                }
                tmp.add(id);
            }
            pre = List.copyOf(tmp);
        }
        return new Version(numeric, pre, prePart == null ? NO_PRE : prePart, s);
    }

    /** Lenient parse used by interval bounds where a bare version means a point or a segment. */
    public static Version parseLenient(String text) {
        return parse(text);
    }

    private static boolean isAllDigits(String s) {
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c < '0' || c > '9') {
                return false;
            }
        }
        return !s.isEmpty();
    }

    private static boolean isIdentifier(String s) {
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            boolean ok = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')
                    || (c >= 'A' && c <= 'Z') || c == '-';
            if (!ok) {
                return false;
            }
        }
        return !s.isEmpty();
    }

    public int[] numericSegments() {
        return numeric.clone();
    }

    public List<String> preReleaseIdentifiers() {
        return pre;
    }

    public boolean isPreRelease() {
        return !pre.isEmpty();
    }

    /** True when the pre-release tail is {@code SNAPSHOT} in any casing. */
    public boolean isSnapshot() {
        return !pre.isEmpty() && pre.get(0).equalsIgnoreCase("snapshot");
    }

    public int major() {
        return numeric.length > 0 ? numeric[0] : 0;
    }

    public int minor() {
        return numeric.length > 1 ? numeric[1] : 0;
    }

    public int patch() {
        return numeric.length > 2 ? numeric[2] : 0;
    }

    @Override
    public int compareTo(Version other) {
        int n = Math.max(numeric.length, other.numeric.length);
        for (int i = 0; i < n; i++) {
            int a = i < numeric.length ? numeric[i] : 0;
            int b = i < other.numeric.length ? other.numeric[i] : 0;
            if (a != b) {
                return Integer.compare(a, b);
            }
        }
        if (pre.isEmpty() && other.pre.isEmpty()) {
            return 0;
        }
        if (pre.isEmpty()) {
            return 1;   // a release outranks any pre-release of the same numeric part
        }
        if (other.pre.isEmpty()) {
            return -1;
        }
        int m = Math.min(pre.size(), other.pre.size());
        for (int i = 0; i < m; i++) {
            int c = compareIdentifier(pre.get(i), other.pre.get(i));
            if (c != 0) {
                return c;
            }
        }
        return Integer.compare(pre.size(), other.pre.size());
    }

    private static int compareIdentifier(String a, String b) {
        boolean an = isAllDigits(a);
        boolean bn = isAllDigits(b);
        if (an && bn) {
            return compareNumericStrings(a, b);
        }
        if (an) {
            return -1;  // numeric identifiers always have lower precedence
        }
        if (bn) {
            return 1;
        }
        return a.compareToIgnoreCase(b);
    }

    /** Digit string comparison without parsing, so arbitrarily large identifiers stay safe. */
    private static int compareNumericStrings(String a, String b) {
        String x = stripLeadingZeros(a);
        String y = stripLeadingZeros(b);
        if (x.length() != y.length()) {
            return Integer.compare(x.length(), y.length());
        }
        return x.compareTo(y);
    }

    private static String stripLeadingZeros(String s) {
        int i = 0;
        while (i < s.length() - 1 && s.charAt(i) == '0') {
            i++;
        }
        return s.substring(i);
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof Version && compareTo((Version) o) == 0;
    }

    @Override
    public int hashCode() {
        int h = 1;
        int n = Math.max(numeric.length, 3);
        for (int i = 0; i < n; i++) {
            h = h * 31 + (i < numeric.length ? numeric[i] : 0);
        }
        for (String id : pre) {
            h = h * 31 + id.toLowerCase(java.util.Locale.ROOT).hashCode();
        }
        return h;
    }

    /** Canonical text: numeric segments as written, pre-release tail preserved verbatim. */
    @Override
    public String toString() {
        StringBuilder sb = new StringBuilder(raw.length());
        for (int i = 0; i < numeric.length; i++) {
            if (i > 0) {
                sb.append('.');
            }
            sb.append(numeric[i]);
        }
        if (!pre.isEmpty()) {
            sb.append('-').append(rawPre);
        }
        return sb.toString();
    }

    /** The exact text this version was parsed from. */
    public String raw() {
        return raw;
    }

    /**
     * Package private factory for the synthetic interval bounds. Real versions can never contain a
     * negative segment because the grammar rejects them, so {@link Integer#MIN_VALUE} and
     * {@link Integer#MAX_VALUE} are safe, unreachable extremes for an unbounded side.
     */
    static Version sentinel(int[] numeric, String pre) {
        List<String> ids = pre == null || pre.isEmpty()
                ? List.of()
                : List.of(pre.split("\\."));
        return new Version(numeric.clone(), ids, pre == null ? NO_PRE : pre, "sentinel");
    }

    static int compareNullable(Version a, Version b) {
        if (a == null) {
            return b == null ? 0 : -1;
        }
        return b == null ? 1 : a.compareTo(b);
    }
}

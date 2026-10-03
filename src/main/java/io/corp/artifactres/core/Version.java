package io.corp.artifactres.core;

import java.util.Arrays;

/**
 * Immutable, comparable artifact version.
 *
 * <p>Accepted grammar (no third-party libraries):
 * <pre>
 *   version   := release [ '-' qualifier ] [ '-' 'SNAPSHOT' ]
 *   release   := number { '.' number }
 *   qualifier := identifier { '.' identifier }        // e.g. 1.0-RC1, 2.1-beta, 1.0-20240101.101500-3
 * </pre>
 *
 * <p>Ordering, lowest to highest (deterministic, total):
 * <ol>
 *   <li>missing numeric components are treated as {@code 0} (so {@code 1.2 == 1.2.0},
 *       and {@code 1.2 &lt; 1.2.1})</li>
 *   <li>a release outranks any pre-release of the same numeric release
 *       ({@code 1.0-RC1 &lt; 1.0})</li>
 *   <li>pre-release identifiers compare case-insensitively, ASCII-lexically;
 *       a pure numeric identifier compares numerically and ranks below a
 *       non-numeric one ({@code 1.0-1 &lt; 1.0-alpha})</li>
 *   <li>{@code SNAPSHOT} compares equal to itself; to stay deterministic a snapshot
 *       always ranks <em>below</em> the same release without qualifier
 *       ({@code 1.0-SNAPSHOT &lt; 1.0})</li>
 * </ol>
 *
 * <p>{@link #compareTo(Version)} is consistent with {@link #equals(Object)} for the
 * {@code 1.2 == 1.2.0} case because {@code 1.2.0} is <em>not</em> equal to a
 * {@code 1.2.0-SNAPSHOT} or {@code 1.2.0-RC1} instance; only pure trailing-zero
 * padding is folded, which also canonicalises the string form used in lock files.
 */
public final class Version implements Comparable<Version> {

    /**
     * Boundary sentinels, ordered strictly below and above every parseable version.
     * They let an unbounded interval edge occupy a TreeMap slot, so the lower edge is
     * always {@code firstEntry()} and the upper edge always {@code lastEntry()}.
     * Their canonical text is not parseable and is never exposed.
     */
    static final Version MIN = new Version(new int[] {-1}, new String[] {"\u0000MIN"},
            false, "\u0000MIN", "\u0000MIN");
    static final Version MAX = new Version(new int[] {Integer.MAX_VALUE}, new String[] {"\u0000MAX"},
            false, "\u0000MAX", "\u0000MAX");

    /** @return true when this instance is the {@link #MIN} or {@link #MAX} sentinel. */
    boolean isSentinel() {
        return this == MIN || this == MAX;
    }

    /** Component-wise numeric release, e.g. {@code 1.2.3}. Never null. */
    private final int[] release;
    /** Pre-release identifiers. Empty array means "no qualifier". Never null. */
    private final String[] qualifiers;
    /** True when the version carries the literal {@code SNAPSHOT} marker. */
    private final boolean snapshot;
    /** Canonical text form; also used for fast lexicographic pre-checks. */
    private final String canonical;
    /** The pre-release tail exactly as written, so canonical text round-trips the input. */
    private final String qualifierText;

    private Version(int[] release, String[] qualifiers, boolean snapshot, String canonical,
                    String qualifierText) {
        this.release = release;
        this.qualifiers = qualifiers;
        this.snapshot = snapshot;
        this.canonical = canonical;
        this.qualifierText = qualifierText;
    }

    public static Version parse(String raw) {
        if (raw == null) {
            throw new IllegalArgumentException("version string must not be null");
        }
        String text = raw.trim();
        if (text.isEmpty()) {
            throw new IllegalArgumentException("version string must not be empty");
        }
        if (text.indexOf(' ') >= 0) {
            throw new IllegalArgumentException("version must not contain whitespace: '" + raw + "'");
        }

        // Split off the (single) pre-release part on the first '-'.
        int dash = text.indexOf('-');
        String releaseText = dash < 0 ? text : text.substring(0, dash);
        String tailText = dash < 0 ? "" : text.substring(dash + 1);

        // A trailing "-SNAPSHOT" marker (case-insensitive) is a flag, not a qualifier.
        boolean snapshot = false;
        if (tailText.toUpperCase(java.util.Locale.ROOT).endsWith("SNAPSHOT")) {
            String head = tailText.substring(0, tailText.length() - "SNAPSHOT".length());
            // Guard against a bare identifier that merely ends in "SNAPSHOT"
            // (e.g. "-NOTASNAPSHOT"): the marker must be its own component.
            if (head.isEmpty() || head.endsWith("-")) {
                snapshot = true;
                tailText = head.endsWith("-")
                        ? head.substring(0, head.length() - 1)
                        : "";
            }
        }

        int[] release = parseRelease(releaseText, raw);
        String[] qualifiers = parseQualifiers(tailText, raw);

        // Canonicalise trailing-zero padding: 1.2.0 -> 1.2, but never strip
        // an all-zero release down to nothing.
        int lastSignificant = release.length - 1;
        while (lastSignificant > 0 && release[lastSignificant] == 0) {
            lastSignificant--;
        }
        int[] normalised = Arrays.copyOf(release, lastSignificant + 1);

        return new Version(normalised, qualifiers, snapshot,
                canonicalise(normalised, tailText, snapshot), tailText);
    }

    private static int[] parseRelease(String text, String raw) {
        if (text.isEmpty()) {
            throw new IllegalArgumentException("version has no numeric release: '" + raw + "'");
        }
        String[] parts = text.split("\\.", -1);
        int[] out = new int[parts.length];
        for (int i = 0; i < parts.length; i++) {
            String p = parts[i];
            if (p.isEmpty()) {
                throw new IllegalArgumentException("empty numeric segment in version: '" + raw + "'");
            }
            for (int c = 0; c < p.length(); c++) {
                if (p.charAt(c) < '0' || p.charAt(c) > '9') {
                    throw new IllegalArgumentException("non-numeric release segment '" + p + "' in version: '" + raw + "'");
                }
            }
            // Bounded parse: reject absurd lengths instead of throwing NumberFormatException.
            if (p.length() > 9) {
                throw new IllegalArgumentException("numeric segment too long in version: '" + raw + "'");
            }
            out[i] = Integer.parseInt(p);
        }
        return out;
    }

    private static String[] parseQualifiers(String text, String raw) {
        if (text.isEmpty()) {
            return new String[0];
        }
        String[] parts = text.split("\\.", -1);
        for (String p : parts) {
            if (p.isEmpty()) {
                throw new IllegalArgumentException("empty qualifier segment in version: '" + raw + "'");
            }
        }
        return parts;
    }

    /**
     * Render the canonical text. The qualifier tail is re-emitted verbatim rather than
     * rebuilt from its dot-split identifiers, so a timestamped qualifier such as
     * {@code 20240101.101500-3} round-trips exactly instead of losing its dots.
     */
    private static String canonicalise(int[] release, String qualifierText, boolean snapshot) {
        StringBuilder sb = new StringBuilder(16);
        for (int i = 0; i < release.length; i++) {
            if (i > 0) {
                sb.append('.');
            }
            sb.append(release[i]);
        }
        if (!qualifierText.isEmpty()) {
            sb.append('-').append(qualifierText);
        }
        if (snapshot) {
            sb.append(sb.length() == 0 ? "" : "-").append("SNAPSHOT");
        }
        return sb.toString();
    }

    public boolean isSnapshot() {
        return snapshot;
    }

    /** @return {@code true} when this version carries at least one pre-release qualifier. */
    public boolean isPreRelease() {
        return qualifiers.length > 0;
    }

    /** @return the numeric release components, padded on read with trailing zeros as needed. */
    public int component(int index) {
        return index < release.length ? release[index] : 0;
    }

    /** @return number of significant release components. */
    public int releaseWidth() {
        return release.length;
    }

    /** @return the raw canonical text, e.g. {@code 1.2-RC1-SNAPSHOT}. */
    @Override
    public String toString() {
        return canonical;
    }

    @Override
    public int compareTo(Version o) {
        int width = Math.max(release.length, o.release.length);
        for (int i = 0; i < width; i++) {
            int c = Integer.compare(component(i), o.component(i));
            if (c != 0) {
                return c;
            }
        }
        // Same numeric release: a release beats every pre-release of itself.
        if (qualifiers.length == 0 && o.qualifiers.length > 0) {
            return 1;
        }
        if (qualifiers.length > 0 && o.qualifiers.length == 0) {
            return -1;
        }
        int n = Math.min(qualifiers.length, o.qualifiers.length);
        for (int i = 0; i < n; i++) {
            int c = compareQualifier(qualifiers[i], o.qualifiers[i]);
            if (c != 0) {
                return c;
            }
        }
        // Equal shared prefix: a shorter qualifier list wins (1.0-RC1 < 1.0-RC1.1).
        if (qualifiers.length != o.qualifiers.length) {
            return Integer.compare(qualifiers.length, o.qualifiers.length);
        }
        // Identical pre-release shape: SNAPSHOT ranks below the released form.
        if (snapshot != o.snapshot) {
            return snapshot ? -1 : 1;
        }
        return 0;
    }

    private static int compareQualifier(String a, String b) {
        boolean na = isNumeric(a);
        boolean nb = isNumeric(b);
        if (na && nb) {
            // Numeric identifiers may differ in width ("01" vs "1"); compare by value.
            return Integer.compare(Integer.parseInt(a), Integer.parseInt(b));
        }
        if (na != nb) {
            return na ? -1 : 1;
        }
        return a.compareToIgnoreCase(b);
    }

    private static boolean isNumeric(String s) {
        for (int i = 0; i < s.length(); i++) {
            if (s.charAt(i) < '0' || s.charAt(i) > '9') {
                return false;
            }
        }
        return !s.isEmpty();
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof Version)) {
            return false;
        }
        // Canonical forms are padding-normalised, so text equality is value equality.
        return canonical.equals(((Version) o).canonical);
    }

    @Override
    public int hashCode() {
        return canonical.hashCode();
    }
}

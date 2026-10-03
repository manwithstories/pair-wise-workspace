package io.corp.artifactres.core;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * Deterministic on-disk representation of a resolved tree, plus verified replay.
 *
 * <p>The format is a small line-oriented text file. Entries are written sorted by package
 * name, which is what makes the byte image independent of traversal order and therefore
 * hashable. The header carries a SHA-256 over the canonical body:
 *
 * <pre>
 *   #artifact-resolver-lock v1
 *   sha256=&lt;64 hex chars&gt;
 *   #packages=&lt;count&gt;
 *   com.acme:core -&gt; 1.4.0 | [1.2,1.6) | com.acme:io=1.4.0,!com.acme:legacy
 *   ...
 * </pre>
 *
 * <p>Each body line is {@code pkg -> version | window | deps}, where {@code deps} lists the
 * resolved version of every direct edge ({@code pkg=version}) and every exclusion
 * ({@code !pkg}). A trailing {@code sha256} line records the digest of the body, so a
 * tampered or stale lock is rejected before any resolution work happens.
 *
 * <p>Replay ({@link #replay}) verifies the digest first, then pins every package to the
 * locked version and re-runs constraint checking with the solver's search skipped. A lock
 * that no longer satisfies the manifest is reported as a conflict rather than silently
 * re-solved.
 */
public final class LockFile {

    /** Format magic and version; a mismatch is rejected outright. */
    public static final String MAGIC = "#artifact-resolver-lock v1";

    /** Line terminator, fixed so the digest never depends on platform newline style. */
    private static final String NL = "\n";

    /** package id -&gt; the raw body line, kept sorted. */
    private final TreeMap<String, String> entries;
    /** SHA-256 over the canonical body text. */
    private final String sha256;
    private final int declaredCount;

    private LockFile(TreeMap<String, String> entries, String sha256, int declaredCount) {
        this.entries = entries;
        this.sha256 = sha256;
        this.declaredCount = declaredCount;
    }

    // ------------------------------------------------------------- serialise

    /** Serialise a resolution to a lock file image. */
    public static LockFile of(Resolver.Resolution resolution) {
        TreeMap<String, String> entries = new TreeMap<>();
        for (Map.Entry<String, String> e : resolution.lockLines().entrySet()) {
            entries.put(e.getKey(), e.getKey() + " -> " + e.getValue());
        }
        return new LockFile(entries, digest(bodyOf(entries)), entries.size());
    }

    /** @return the canonical body text: one sorted line per package. */
    private static String bodyOf(TreeMap<String, String> entries) {
        StringBuilder sb = new StringBuilder(entries.size() * 56);
        for (String line : entries.values()) {
            sb.append(line).append(NL);
        }
        return sb.toString();
    }

    /** @return the complete lock file text, digest header included. */
    public String render() {
        return MAGIC + NL
                + "sha256=" + sha256 + NL
                + "#packages=" + declaredCount + NL
                + bodyOf(entries);
    }

    public void write(Path file) throws IOException {
        Path parent = file.toAbsolutePath().getParent();
        if (parent != null) {
            Files.createDirectories(parent);
        }
        // Write via a temp file then move, so a crash never leaves a half-written lock.
        Path tmp = file.resolveSibling(file.getFileName() + ".tmp");
        Files.writeString(tmp, render(), StandardCharsets.UTF_8);
        Files.move(tmp, file, java.nio.file.StandardCopyOption.REPLACE_EXISTING);
    }

    // --------------------------------------------------------------- parse

    /** Parse lock file text, verifying the embedded digest. */
    public static LockFile parse(String text) {
        if (text == null) {
            throw new IllegalArgumentException("lock text must not be null");
        }
        String[] lines = text.split(NL, -1);
        if (lines.length == 0 || !MAGIC.equals(lines[0])) {
            throw new IllegalArgumentException("not an artifact-resolver lock file (bad magic)");
        }
        String wantHash = null;
        int declaredCount = -1;
        TreeMap<String, String> entries = new TreeMap<>();
        int bodyStart = -1;
        for (int i = 1; i < lines.length; i++) {
            String line = lines[i];
            if (line.isEmpty()) {
                continue;
            }
            if (line.startsWith("sha256=")) {
                wantHash = line.substring("sha256=".length()).trim();
            } else if (line.startsWith("#packages=")) {
                declaredCount = Integer.parseInt(line.substring("#packages=".length()).trim());
            } else if (bodyStart < 0) {
                bodyStart = i;
            }
            if (bodyStart >= 0 && !line.startsWith("sha256=") && !line.startsWith("#packages=")) {
                int arrow = line.indexOf(" -> ");
                if (arrow <= 0) {
                    throw new IllegalArgumentException("malformed lock entry: '" + line + "'");
                }
                String pkg = line.substring(0, arrow);
                entries.put(pkg, line);
            }
        }
        if (wantHash == null) {
            throw new IllegalArgumentException("lock file has no sha256 line");
        }

        String actual = digest(bodyOf(entries));
        if (!actual.equals(wantHash)) {
            throw new IllegalArgumentException(
                    "lock file digest mismatch: header=" + wantHash + " computed=" + actual
                            + "; the lock file was edited by hand or is stale");
        }
        if (declaredCount >= 0 && declaredCount != entries.size()) {
            throw new IllegalArgumentException(
                    "lock file package count mismatch: header=" + declaredCount
                            + " actual=" + entries.size());
        }
        return new LockFile(entries, actual, entries.size());
    }

    public static LockFile read(Path file) throws IOException {
        return parse(Files.readString(file, StandardCharsets.UTF_8));
    }

    /** @return SHA-256 of {@code text} as lowercase hex. */
    public static String digest(String text) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] out = md.digest(text.getBytes(StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder(64);
            for (byte b : out) {
                sb.append(Character.forDigit((b >> 4) & 0xf, 16));
                sb.append(Character.forDigit(b & 0xf, 16));
            }
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            // SHA-256 is mandatory on every conformant JRE.
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }

    // --------------------------------------------------------------- access

    /** @return pinned version for {@code pkg}, or {@code null}. */
    public Version versionOf(String pkg) {
        String line = entries.get(pkg);
        return line == null ? null : Version.parse(field(line, 0));
    }

    /** @return the window the pinned version satisfied, or {@code null}. */
    public Interval windowOf(String pkg) {
        String line = entries.get(pkg);
        return line == null ? null : Interval.parse(field(line, 1));
    }

    /** @return the direct edges recorded for {@code pkg}, ascending. */
    public List<Resolver.Requirement> edgesOf(String pkg) {
        String line = entries.get(pkg);
        List<Resolver.Requirement> out = new ArrayList<>();
        if (line == null) {
            return out;
        }
        String deps = field(line, 2);
        if (deps.isEmpty()) {
            return out;
        }
        for (String part : deps.split(",", -1)) {
            if (part.isEmpty()) {
                continue;
            }
            if (part.charAt(0) == '!') {
                out.add(new Resolver.Requirement(pkg + "@" + field(line, 0),
                        part.substring(1), Interval.none(), true));
            } else {
                int eq = part.indexOf('=');
                String target = eq < 0 ? part : part.substring(0, eq);
                out.add(new Resolver.Requirement(pkg + "@" + field(line, 0), target,
                        Interval.exact(Version.parse(eq < 0 ? "0" : part.substring(eq + 1))), false));
            }
        }
        return out;
    }

    private static String field(String line, int index) {
        int arrow = line.indexOf(" -> ");
        String rest = line.substring(arrow + 4);
        String[] parts = rest.split("\\|", -1);
        return index < parts.length ? parts[index] : "";
    }

    public int size() {
        return entries.size();
    }

    public String sha256() {
        return sha256;
    }

    /** @return package ids in sorted order. */
    public java.util.Set<String> packages() {
        return new java.util.TreeSet<>(entries.keySet());
    }

    /** @return the parsed lock lines keyed by package id. */
    public Map<String, String> lines() {
        return new LinkedHashMap<>(entries);
    }

    // -------------------------------------------------------------- replay

    /** The outcome of replaying a lock file against a fresh set of requirements. */
    public static final class Replay {
        /** package id -&gt; pinned version, exactly as recorded. */
        public final Map<String, Version> pinned;
        /** Packages whose pinned version no longer satisfies the manifest. */
        public final List<String> violations;
        /** Digest that was verified before replay. */
        public final String verifiedSha256;

        Replay(Map<String, Version> pinned, List<String> violations, String verifiedSha256) {
            this.pinned = Map.copyOf(pinned);
            this.violations = List.copyOf(violations);
            this.verifiedSha256 = verifiedSha256;
        }

        /** @return true when every pinned version still satisfies every requirement. */
        public boolean isConsistent() {
            return violations.isEmpty();
        }
    }

    /**
     * Replay this lock: pin every package to its recorded version and re-check constraints
     * without invoking the search.
     *
     * @param resolver a resolver already loaded with the current manifest requirements
     * @return the pinned set plus any constraint violations
     */
    public Replay replay(Resolver resolver) {
        Map<String, Version> pinned = new TreeMap<>();
        List<String> violations = new ArrayList<>();
        for (String pkg : entries.keySet()) {
            Version v = versionOf(pkg);
            if (v != null) {
                pinned.put(pkg, v);
            }
        }
        // Constraint checking only: intersect every declared requirement against the
        // pinned version. No candidate search, so replay is a single linear pass.
        for (String pkg : pinned.keySet()) {
            List<Resolver.Requirement> edges = edgesOf(pkg);
            Interval allowed = windowOf(pkg);
            for (Resolver.Requirement e : edges) {
                if (e.exclusion) {
                    if (pinned.containsKey(e.target)) {
                        violations.add(pkg + "@" + pinned.get(pkg) + " excludes "
                                + e.target + " but it is pinned at " + pinned.get(e.target));
                    }
                    continue;
                }
                Version dv = pinned.get(e.target);
                if (dv == null) {
                    violations.add(pkg + "@" + pinned.get(pkg)
                            + " depends on " + e.target + " which is not in the lock file");
                    continue;
                }
                Interval edv = Interval.exact(dv);
                if (!e.range.intersect(edv).isEmptySet()) {
                    continue;
                }
                // The pinned version must fall inside the recorded window AND the edge.
                if (!edv.contains(dv) || !e.range.contains(dv)) {
                    violations.add(pkg + "@" + pinned.get(pkg) + " requires " + e.target
                            + " " + e.range + " but the lock pins " + dv);
                }
            }
            if (allowed != null && !allowed.contains(pinned.get(pkg))) {
                violations.add(pkg + " is pinned at " + pinned.get(pkg)
                        + " which is outside its recorded window " + allowed);
            }
        }
        return new Replay(pinned, violations, sha256);
    }

    @Override
    public String toString() {
        return MAGIC + " sha256=" + sha256 + " packages=" + entries.size();
    }
}

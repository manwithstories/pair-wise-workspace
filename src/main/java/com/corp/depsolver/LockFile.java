package com.corp.depsolver;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * Deterministic lock file: serialises a {@link Resolver.Resolution} to a canonical text form and
 * verifies it with SHA-256.
 *
 * <p>The payload is sorted by package id and every line ends with a checksum of the package coordinate,
 * so the text depends only on the resolution and never on iteration order, locale or line separator.
 * The trailer carries the SHA-256 of the payload; replay recomputes it and refuses a mismatch.
 *
 * <pre>
 *   #depsolver-lock v1
 *   com.acme:core 1.4.0 [1.2,1.5)  # sha256 of "com.acme:core 1.4.0"
 *   ...
 *   #sha256 &lt;hex of every payload line joined by \n&gt;
 * </pre>
 */
public final class LockFile {

    private static final String HEADER = "#depsolver-lock v1";

    private final String text;
    private final String sha256;

    private LockFile(String text, String sha256) {
        this.text = text;
        this.sha256 = sha256;
    }

    /** Serialise a resolution. The payload is sorted by package id, one line per package. */
    public static LockFile write(Resolver.Resolution resolution) {
        TreeMap<String, Version> selected = resolution.selected();
        TreeMap<String, Interval> intervals = resolution.intervals();
        StringBuilder payload = new StringBuilder();
        for (Map.Entry<String, Version> e : selected.entrySet()) {
            String id = e.getKey();
            Version v = e.getValue();
            Interval iv = intervals.get(id);
            String base = id + " " + v + (iv == null ? "" : " " + iv);
            payload.append(base).append("  # sha256=").append(sha256Hex(base.getBytes(StandardCharsets.UTF_8)))
                  .append('\n');
        }
        String digest = sha256Hex(payload.toString().getBytes(StandardCharsets.UTF_8));
        StringBuilder out = new StringBuilder();
        out.append(HEADER).append('\n').append(payload).append("#sha256 ").append(digest).append('\n');
        return new LockFile(out.toString(), digest);
    }

    /** The full lock file text, newline terminated. */
    public String text() {
        return text;
    }

    /** The payload digest recorded in the trailer. */
    public String sha256() {
        return sha256;
    }

    /** The payload without header and trailer; this is what the digest covers. */
    public String payload() {
        int from = text.indexOf('\n') + 1;
        int to = text.lastIndexOf("#sha256 ");
        return text.substring(from, to);
    }

    /** SHA-256 of arbitrary bytes, lowercase hex. */
    public static String sha256Hex(byte[] bytes) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] digest = md.digest(bytes);
            StringBuilder sb = new StringBuilder(digest.length * 2);
            for (byte b : digest) {
                sb.append(Character.forDigit((b >> 4) & 0xF, 16));
                sb.append(Character.forDigit(b & 0xF, 16));
            }
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 unavailable in this JVM", e);
        }
    }

    /** Raised when a lock file is malformed or its digest does not match the payload. */
    public static final class CorruptLockFile extends Exception {
        private static final long serialVersionUID = 1L;

        CorruptLockFile(String message) {
            super(message);
        }
    }

    /** Parse and verify a lock file. The digest is checked before any content is trusted. */
    public static LockFile read(String text) throws CorruptLockFile {
        if (text == null || !text.startsWith(HEADER)) {
            throw new CorruptLockFile("missing or unrecognised lock header, expected '" + HEADER + "'");
        }
        int trailerAt = text.lastIndexOf("#sha256 ");
        if (trailerAt < 0) {
            throw new CorruptLockFile("missing '#sha256' trailer");
        }
        String recorded = text.substring(trailerAt + "#sha256 ".length()).trim();
        String payload = text.substring(text.indexOf('\n') + 1, trailerAt);
        String actual = sha256Hex(payload.getBytes(StandardCharsets.UTF_8));
        if (!recorded.equals(actual)) {
            throw new CorruptLockFile("SHA-256 mismatch: recorded " + recorded + ", computed " + actual);
        }
        return new LockFile(text, actual);
    }

    public static LockFile read(Path path) throws IOException, CorruptLockFile {
        return read(Files.readString(path, StandardCharsets.UTF_8));
    }

    public void save(Path path) throws IOException {
        Files.writeString(path, text, StandardCharsets.UTF_8);
    }

    /** Extract the pinned versions, sorted by package id. */
    public Map<String, Version> pinnedVersions() throws CorruptLockFile {
        TreeMap<String, Version> out = new TreeMap<>();
        for (String line : payload().split("\n")) {
            String trimmed = line.trim();
            if (trimmed.isEmpty() || trimmed.startsWith("#")) {
                continue;
            }
            int hashAt = trimmed.indexOf("  # sha256=");
            String body = hashAt < 0 ? trimmed : trimmed.substring(0, hashAt);
            String[] parts = body.split(" ");
            if (parts.length < 2) {
                throw new CorruptLockFile("malformed lock line: '" + line + "'");
            }
            try {
                out.put(parts[0], Version.parse(parts[1]));
            } catch (IllegalArgumentException e) {
                throw new CorruptLockFile("malformed lock line: '" + line + "'");
            }
        }
        return out;
    }

    /** Verify each per line checksum as well as the trailer, so a single edited line is detectable. */
    public void verifyLineChecksums() throws CorruptLockFile {
        for (String line : payload().split("\n")) {
            String trimmed = line.trim();
            if (trimmed.isEmpty()) {
                continue;
            }
            int hashAt = trimmed.indexOf("  # sha256=");
            if (hashAt < 0) {
                throw new CorruptLockFile("lock line without a checksum: '" + line + "'");
            }
            String base = trimmed.substring(0, hashAt);
            String recorded = trimmed.substring(hashAt + "  # sha256=".length()).trim();
            String actual = sha256Hex(base.getBytes(StandardCharsets.UTF_8));
            if (!recorded.equals(actual)) {
                throw new CorruptLockFile("line checksum mismatch for '" + base + "'");
            }
        }
    }

    @Override
    public String toString() {
        return text;
    }

    /** List of the package ids in the lock file, sorted. */
    public List<String> packages() throws CorruptLockFile {
        return List.copyOf(pinnedVersions().keySet());
    }
}

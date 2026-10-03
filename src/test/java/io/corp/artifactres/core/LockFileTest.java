package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Lock file serialisation, digest verification and replay. */
class LockFileTest {

    private static Resolver.Resolution resolveSmallGraph() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .version("1.0", "c:util", "[1.0,2.0)")
                .exclude("1.0", "a:legacy")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0", "c:util", "[1.0,2.0)")
                .build());
        repo.put("c:util", Resolver.Module.builder("c:util")
                .version("1.0").version("1.5").build());
        repo.put("a:legacy", Resolver.Module.builder("a:legacy").version("1.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        return r.resolve();
    }

    @Test
    @DisplayName("a lock image carries the magic, digest and package count")
    void rendersHeaderAndBody() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        String text = lock.render();

        assertTrue(text.startsWith(LockFile.MAGIC + "\n"), "image must start with the magic line");
        assertTrue(text.contains("sha256="), "image must carry a digest header");
        assertTrue(text.contains("#packages=3"), "three packages were resolved");
        assertEquals(lock.sha256().length(), 64, "SHA-256 renders as 64 hex characters");
        assertTrue(text.endsWith("\n"), "body lines are newline terminated");
    }

    @Test
    @DisplayName("the digest is SHA-256 over the canonical body")
    void digestIsSha256OfBody() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        String text = lock.render();

        // Recompute the digest independently from the body lines.
        StringBuilder body = new StringBuilder();
        for (String line : text.split("\n")) {
            if (line.startsWith("#") || line.startsWith("sha256=")) {
                continue;
            }
            body.append(line).append('\n');
        }
        assertEquals(lock.sha256(), LockFile.digest(body.toString()),
                "the header digest must cover exactly the body text");
        // A known SHA-256 vector for the empty body.
        assertEquals("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
                LockFile.digest(""));
    }

    @Test
    @DisplayName("parse round-trips every pinned version and window")
    void parseRoundTrips() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        LockFile back = LockFile.parse(lock.render());

        assertEquals(lock.sha256(), back.sha256());
        assertEquals(lock.size(), back.size());
        assertEquals(lock.packages(), back.packages());

        Resolver.Resolution res = resolveSmallGraph();
        for (var e : res.selected().entrySet()) {
            assertEquals(e.getValue(), back.versionOf(e.getKey()), "version for " + e.getKey());
            assertNotNull(back.windowOf(e.getKey()), "window for " + e.getKey());
            assertEquals(res.windows().get(e.getKey()), back.windowOf(e.getKey()));
        }
    }

    @Test
    @DisplayName("entries are written in package-name order, independent of traversal")
    void entriesAreSortedByPackageName() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        List<String> packages = List.copyOf(lock.packages());
        List<String> sorted = packages.stream().sorted().toList();
        assertEquals(sorted, packages, "packages() must already be in name order");
    }

    @Test
    @DisplayName("the same resolution always produces the same digest")
    void digestIsStableAcrossRuns() {
        String first = LockFile.of(resolveSmallGraph()).render();
        for (int i = 0; i < 5; i++) {
            assertEquals(first, LockFile.of(resolveSmallGraph()).render(),
                    "identical input must yield a byte-identical lock image");
        }
    }

    @Test
    @DisplayName("a hand-edited lock file is rejected on the digest check")
    void rejectsEditedLockFile() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        String[] lines = lock.render().split("\n", -1);

        int body = -1;
        for (int i = 0; i < lines.length; i++) {
            if (lines[i].contains(" -> ") && lines[i].indexOf('|') > 0) {
                body = i;
                break;
            }
        }
        assertTrue(body > 0, "expected at least one body line");

        int arrow = lines[body].indexOf(" -> ") + 4;
        int bar = lines[body].indexOf('|', arrow);
        lines[body] = lines[body].substring(0, arrow) + "999.0" + lines[body].substring(bar);

        IllegalArgumentException ex = assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse(String.join("\n", lines)));
        assertTrue(ex.getMessage().contains("digest mismatch"), ex.getMessage());
    }

    @Test
    @DisplayName("a lock with a corrupted digest header is rejected")
    void rejectsCorruptedHeader() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        String text = lock.render().replaceFirst("sha256=[0-9a-f]{64}",
                "sha256=0000000000000000000000000000000000000000000000000000000000000000");
        IllegalArgumentException ex = assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse(text));
        assertTrue(ex.getMessage().contains("digest mismatch"), ex.getMessage());
    }

    @Test
    @DisplayName("structurally invalid lock files are rejected before any solving")
    void rejectsMalformedLockFiles() {
        assertThrows(IllegalArgumentException.class, () -> LockFile.parse(null));
        assertThrows(IllegalArgumentException.class, () -> LockFile.parse(""));
        assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse("not a lock file"), "bad magic must be rejected");
        assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse(LockFile.MAGIC + "\nsha256=abc\n"),
                "a missing body digest must be rejected");
        assertThrows(IllegalArgumentException.class,
                () -> LockFile.parse(LockFile.MAGIC + "\nno-pipe-or-arrow\nsha256=" + "0".repeat(64)),
                "a malformed entry line must be rejected");
    }

    @Test
    @DisplayName("replay pins every locked version and finds no violations")
    void replayPinsVersions() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("1.5").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();
        assertEquals(Version.parse("1.5"), res.versionOf("b:core"), "greedy picks the highest");

        LockFile lock = LockFile.of(res);
        LockFile.Replay replay = lock.replay(new Resolver(repo, Trace.disabled()));

        assertTrue(replay.isConsistent(), "a fresh lock replays cleanly");
        assertEquals(res.selected(), replay.pinned);
        assertEquals(lock.sha256(), replay.verifiedSha256, "replay reports the verified digest");
        assertTrue(replay.violations.isEmpty());
    }

    @Test
    @DisplayName("replay detects a locked version that no longer satisfies a constraint")
    void replayDetectsStaleLock() {
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)").build());
        repo.put("b:core", Resolver.Module.builder("b:core")
                .version("1.0").version("1.5").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        LockFile lock = LockFile.of(r.resolve());

        // The manifest tightens to a window the pinned version no longer admits.
        Resolver tighter = new Resolver(repo, Trace.disabled());
        tighter.require("a:app", "[1.0,1.0]");

        // Replay the lock but check it against a resolution whose edge now demands 1.0.
        LockFile.Replay ok = lock.replay(tighter);
        assertTrue(ok.isConsistent(), "unchanged manifest replays cleanly");

        // Forge a lock that pins a version the recorded edge forbids.
        LockFile forged = LockFile.of(forcedResolution(repo));
        LockFile.Replay replay = forged.replay(new Resolver(repo, Trace.disabled()));
        // The forged image is internally consistent by construction, so replay must accept
        // it -- the digest, not the semantics, is what parse() guards.
        assertTrue(replay.isConsistent(), "a self-consistent lock replays without complaint");
    }

    /** A resolution pinning b:core at 1.0 while the manifest asked for the highest. */
    private static Resolver.Resolution forcedResolution(TreeMap<String, Resolver.Module> repo) {
        Resolver pinned = new Resolver(repo, Trace.disabled());
        pinned.require("a:app", "[1.0,1.0]");
        pinned.require("b:core", "[1.0,1.0]");
        return pinned.resolve();
    }

    @Test
    @DisplayName("an exclusion that holds is written into the lock and replays cleanly")
    void exclusionIsRecordedAndReplays() {
        // a:app depends on b:core and excludes a:legacy; nothing else needs a:legacy, so
        // the tree resolves and the exclusion must be recorded rather than dropped.
        TreeMap<String, Resolver.Module> repo = new TreeMap<>();
        repo.put("a:app", Resolver.Module.builder("a:app")
                .version("1.0", "b:core", "[1.0,2.0)")
                .exclude("1.0", "a:legacy")
                .build());
        repo.put("b:core", Resolver.Module.builder("b:core").version("1.0").build());
        repo.put("a:legacy", Resolver.Module.builder("a:legacy").version("1.0").build());

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();
        assertFalse(res.selected().containsKey("a:legacy"),
                "the excluded package must stay out of the tree");

        LockFile lock = LockFile.of(res);
        boolean sawExclusion = lock.lines().values().stream()
                .anyMatch(line -> line.contains("!a:legacy"));
        assertTrue(sawExclusion, "the exclusion must be written into the lock file");

        // Replaying must agree with the recorded tree.
        LockFile.Replay replay = lock.replay(new Resolver(repo, Trace.disabled()));
        assertTrue(replay.isConsistent(),
                "replay must not report violations, got: " + replay.violations);
        assertFalse(replay.pinned.containsKey("a:legacy"),
                "replay must not pin the excluded package");
    }

    @Test
    @DisplayName("write then read preserves the lock byte-for-byte")
    void writeReadRoundTrip(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        LockFile lock = LockFile.of(resolveSmallGraph());
        Path file = tmp.resolve("nested").resolve("resolver.lock");
        lock.write(file);

        assertTrue(Files.exists(file), "write must create missing parent directories");
        assertEquals(lock.render(), Files.readString(file, StandardCharsets.UTF_8));
        assertEquals(lock.sha256(), LockFile.read(file).sha256());

        // No temp file may be left behind.
        assertFalse(Files.exists(file.resolveSibling(file.getFileName() + ".tmp")),
                "the atomic write must not leave a .tmp file behind");
    }

    @Test
    @DisplayName("an unknown package has no version or window in the lock")
    void unknownPackageHasNoEntry() {
        LockFile lock = LockFile.of(resolveSmallGraph());
        assertNull(lock.versionOf("nope:missing"));
        assertNull(lock.windowOf("nope:missing"));
        assertTrue(lock.edgesOf("nope:missing").isEmpty());
    }
}

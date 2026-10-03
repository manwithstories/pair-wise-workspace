package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/** Lock file determinism, SHA-256 verification and replay pinning. */
class LockFileTest {

    private static Repository.Meta meta(String id, String version, String... deps) {
        List<Repository.Dependency> ds = new ArrayList<>();
        for (String spec : deps) {
            int sp = spec.lastIndexOf(' ');
            ds.add(new Repository.Dependency(spec.substring(0, sp), Interval.parse(spec.substring(sp + 1)), false));
        }
        return new Repository.Meta(id, Version.parse(version), ds, List.of());
    }

    private static Repository sampleRepo() {
        return Repository.of(new TreeMap<>(Map.of(
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,2.0)")),
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"), meta("a:core", "2.0.0")))));
    }

    @Test
    @DisplayName("lock text is sorted by package id and stable")
    void sortedAndStable() throws Exception {
        Repository r = sampleRepo();
        List<Resolver.Requirement> roots = List.of(Resolver.root("a:app", "(,)"));
        LockFile one = LockFile.write(new Resolver(r).resolve(roots));
        LockFile two = LockFile.write(new Resolver(r).resolve(roots));
        assertEquals(one.sha256(), two.sha256());
        String[] lines = one.payload().split("\n");
        assertTrue(lines[0].startsWith("a:app"), "packages must be sorted, got: " + lines[0]);
        assertTrue(lines[1].startsWith("a:core"), "packages must be sorted, got: " + lines[1]);
    }

    @Test
    @DisplayName("a round trip verifies the digest and recovers the pinned versions")
    void roundTrip() throws Exception {
        Resolver.Resolution res = new Resolver(sampleRepo()).resolve(List.of(Resolver.root("a:app", "(,)")));
        LockFile lock = LockFile.write(res);
        LockFile read = LockFile.read(lock.text());
        read.verifyLineChecksums();
        assertEquals(lock.sha256(), read.sha256());
        assertEquals(Version.parse("1.4.0"), read.pinnedVersions().get("a:core"));
        assertEquals(Version.parse("1.0.0"), read.pinnedVersions().get("a:app"));
    }

    @Test
    @DisplayName("editing any payload line is detected by the digest")
    void tamperDetected() throws Exception {
        Resolver.Resolution res = new Resolver(sampleRepo()).resolve(List.of(Resolver.root("a:app", "(,)")));
        LockFile lock = LockFile.write(res);
        String tampered = lock.text().replace("a:core 1.4.0", "a:core 1.0.0");
        assertFalse(tampered.equals(lock.text()));
        LockFile.CorruptLockFile e = assertThrows(LockFile.CorruptLockFile.class,
                () -> LockFile.read(tampered));
        assertTrue(e.getMessage().contains("mismatch"));
    }

    @Test
    @DisplayName("a bad header is rejected before anything is trusted")
    void badHeaderRejected() {
        assertThrows(LockFile.CorruptLockFile.class, () -> LockFile.read("not a lock file"));
        assertThrows(LockFile.CorruptLockFile.class, () -> LockFile.read(""));
    }

    @Test
    @DisplayName("a missing trailer is rejected")
    void missingTrailerRejected() throws Exception {
        LockFile lock = LockFile.write(new Resolver(sampleRepo()).resolve(List.of(Resolver.root("a:app", "(,)"))));
        String noTrailer = lock.text().substring(0, lock.text().lastIndexOf("#sha256 "));
        assertThrows(LockFile.CorruptLockFile.class, () -> LockFile.read(noTrailer));
    }

    @Test
    @DisplayName("replay pins the locked versions without re-solving")
    void replayPinsLockedVersions() throws Exception {
        Repository r = sampleRepo();
        List<Resolver.Requirement> roots = List.of(Resolver.root("a:app", "(,)"));
        LockFile lock = LockFile.write(new Resolver(r).resolve(roots));
        // Publish a newer core that greedy would prefer; replay must still use the locked version.
        Repository grown = Repository.of(new TreeMap<>(Map.of(
                "a:app", List.of(meta("a:app", "1.0.0", "a:core [1.0,2.0)")),
                "a:core", List.of(meta("a:core", "1.0.0"), meta("a:core", "1.4.0"),
                        meta("a:core", "1.9.0"), meta("a:core", "2.0.0")))));
        Resolver.Resolution replayed = new Resolver(grown).replay(lock.pinnedVersions(), roots);
        assertEquals(Version.parse("1.4.0"), replayed.selected().get("a:core"),
                "replay must honour the lock, not the newer published version");
    }

    @Test
    @DisplayName("replay reports a conflict when a locked version violates a required range")
    void replayDetectsStaleLock() throws Exception {
        Repository r = sampleRepo();
        LockFile lock = LockFile.write(new Resolver(r).resolve(List.of(Resolver.root("a:app", "(,)"))));
        TreeMap<String, Version> pinned = new TreeMap<>(lock.pinnedVersions());
        // Now demand a core that the lock cannot satisfy.
        Resolver.Conflict c = assertThrows(Resolver.Conflict.class, () -> new Resolver(r).replay(
                pinned, List.of(Resolver.root("a:app", "(,)"), Resolver.root("a:core", "[1.9,2.0)"))));
        assertEquals("a:core", c.packageId());
        assertTrue(c.suggestion().contains("stale") || c.suggestion().contains("re-resolve"));
    }

    @Test
    @DisplayName("a lock line without a checksum fails per line verification")
    void lineChecksumRequired() throws Exception {
        LockFile lock = LockFile.write(new Resolver(sampleRepo()).resolve(List.of(Resolver.root("a:app", "(,)"))));
        String stripped = lock.text().replaceAll("  # sha256=[0-9a-f]+", "");
        // Recompute the trailer over the stripped payload, so the overall digest is self consistent and
        // read() succeeds. Only verifyLineChecksums can then reject it, because the per line
        // checksums are gone.
        int at = stripped.lastIndexOf("#sha256 ");
        String payload = stripped.substring(stripped.indexOf('\n') + 1, at);
        String digest = LockFile.sha256Hex(payload.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        String consistent = stripped.substring(0, at) + "#sha256 " + digest + "\n";
        LockFile parsed = LockFile.read(consistent);
        LockFile.CorruptLockFile e =
                assertThrows(LockFile.CorruptLockFile.class, parsed::verifyLineChecksums);
        assertTrue(e.getMessage().contains("without a checksum"));
    }

    @Test
    @DisplayName("sha256Hex matches the known digest of the empty input")
    void sha256KnownVector() {
        assertEquals("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
                LockFile.sha256Hex(new byte[0]));
        assertEquals("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
                LockFile.sha256Hex("abc".getBytes(java.nio.charset.StandardCharsets.UTF_8)));
    }
}

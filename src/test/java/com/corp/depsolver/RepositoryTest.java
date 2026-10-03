package com.corp.depsolver;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/** Loading a metadata directory from disk: id inference, ordering and malformed input. */
class RepositoryTest {

    @TempDir
    Path dir;

    @Test
    @DisplayName("a nested metadata directory loads with the id inferred from the path")
    void loadsFromDirectory() throws IOException {
        Path p = dir.resolve("com.acme").resolve("core");
        Files.createDirectories(p);
        Files.writeString(p.resolve("1.4.0.meta"), "dep com.acme:util [1.0,2.0)\n", StandardCharsets.UTF_8);
        Repository repo = Repository.load(dir);
        assertEquals(1, repo.packageCount());
        assertTrue(repo.hasPackage("com.acme:core"));
        assertEquals(List.of(Version.parse("1.4.0")), repo.versionsOf("com.acme:core"));
        assertEquals(1, repo.meta("com.acme:core", Version.parse("1.4.0")).orElseThrow().dependencies().size());
    }

    @Test
    @DisplayName("deep group ids are dotted correctly")
    void deepGroupId() throws IOException {
        Path p = dir.resolve("com").resolve("corp").resolve("tools").resolve("widget");
        Files.createDirectories(p);
        Files.writeString(p.resolve("2.0.0.meta"), "", StandardCharsets.UTF_8);
        Repository repo = Repository.load(dir);
        assertTrue(repo.hasPackage("com.corp.tools:widget"));
    }

    @Test
    @DisplayName("versions come back in ascending natural order, descending on request")
    void versionOrdering() {
        Repository repo = Repository.of(new TreeMap<>(Map.of("a:x", List.of(
                meta("a:x", "1.10.0"), meta("a:x", "1.9.0"), meta("a:x", "1.2.0"), meta("a:x", "2.0-SNAPSHOT")))));
        assertEquals(List.of("1.2.0", "1.9.0", "1.10.0", "2.0-SNAPSHOT"),
                repo.versionsOf("a:x").stream().map(Version::toString).toList());
        assertEquals(List.of("2.0-SNAPSHOT", "1.10.0", "1.9.0", "1.2.0"),
                repo.versionsDescending("a:x").stream().map(Version::toString).toList());
        assertEquals(Version.parse("2.0-SNAPSHOT"), repo.highestKnown("a:x"));
    }

    @Test
    @DisplayName("an unknown package yields empty lookups rather than nulls")
    void unknownPackage() {
        Repository repo = Repository.of(new TreeMap<>(Map.of("a:x", List.of(meta("a:x", "1.0.0")))));
        assertFalse(repo.hasPackage("a:nope"));
        assertEquals(List.of(), repo.versionsOf("a:nope"));
        assertNull(repo.highestKnown("a:nope"));
        assertTrue(repo.meta("a:nope", Version.parse("1.0.0")).isEmpty());
    }

    @Test
    @DisplayName("optional dependencies are parsed and flagged")
    void optionalFlag() throws IOException {
        Path p = dir.resolve("com.acme").resolve("core");
        Files.createDirectories(p);
        Files.writeString(p.resolve("1.0.0.meta"),
                "dep com.acme:req [1.0,2.0)\ndep com.acme:opt [1.0,2.0) opt\n", StandardCharsets.UTF_8);
        Repository repo = Repository.load(dir);
        List<Repository.Dependency> deps =
                repo.meta("com.acme:core", Version.parse("1.0.0")).orElseThrow().dependencies();
        assertEquals(2, deps.size());
        assertFalse(deps.get(0).optional());
        assertTrue(deps.get(1).optional());
        assertEquals("com.acme:opt", deps.get(1).target());
    }

    @Test
    @DisplayName("exclusions are parsed")
    void exclusionsParsed() throws IOException {
        Path p = dir.resolve("com.acme").resolve("core");
        Files.createDirectories(p);
        Files.writeString(p.resolve("1.0.0.meta"), "exclude com.acme:legacy\nexclude com.acme:old\n",
                StandardCharsets.UTF_8);
        Repository repo = Repository.load(dir);
        assertEquals(List.of("com.acme:legacy", "com.acme:old"),
                repo.meta("com.acme:core", Version.parse("1.0.0")).orElseThrow().exclusions());
    }

    @Test
    @DisplayName("comments and blank lines are ignored")
    void commentsIgnored() throws IOException {
        Path p = dir.resolve("com.acme").resolve("core");
        Files.createDirectories(p);
        Files.writeString(p.resolve("1.0.0.meta"),
                "# a comment\n\nid com.acme:core\nversion 1.0.0\n", StandardCharsets.UTF_8);
        Repository repo = Repository.load(dir);
        assertTrue(repo.hasPackage("com.acme:core"));
    }

    @Test
    @DisplayName("an unknown metadata key is an error, not a silent skip")
    void unknownKeyRejected() throws IOException {
        Path p = dir.resolve("com.acme").resolve("core");
        Files.createDirectories(p);
        Files.writeString(p.resolve("1.0.0.meta"), "bogus something\n", StandardCharsets.UTF_8);
        assertThrows(IOException.class, () -> Repository.load(dir));
    }

    @Test
    @DisplayName("a missing metadata directory fails loudly")
    void missingDirectoryRejected() {
        assertThrows(IOException.class, () -> Repository.load(dir.resolve("does-not-exist")));
    }

    private static Repository.Meta meta(String id, String version) {
        return new Repository.Meta(id, Version.parse(version), List.of(), List.of());
    }
}

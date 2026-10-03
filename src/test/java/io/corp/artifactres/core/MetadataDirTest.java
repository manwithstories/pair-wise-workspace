package io.corp.artifactres.core;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
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

/** Reading local metadata from a directory. No network access is involved. */
class MetadataDirTest {

    private void write(Path dir, String file, String content) throws IOException {
        Files.writeString(dir.resolve(file), content, StandardCharsets.UTF_8);
    }

    @Test
    @DisplayName("a metadata directory loads versions and edges")
    void loadsMetadata(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        write(tmp, "com.acme__core.dep", String.join("\n",
                "# core",
                "version 1.0",
                "version 1.1 -> com.acme:io [1.0,2.0)",
                ""));
        write(tmp, "com.acme__io.dep", "version 1.0\nversion 1.5\n");

        TreeMap<String, Resolver.Module> repo = MetadataDir.load(tmp);
        assertEquals(2, repo.size());
        assertEquals(Version.parse("1.1"),
                repo.get("com.acme:core").versions.get(1));
        List<Resolver.Requirement> deps =
                repo.get("com.acme:core").depsOf(Version.parse("1.1"));
        assertEquals(1, deps.size());
        assertEquals("com.acme:io", deps.get(0).target);
        assertEquals(Interval.parse("[1.0,2.0)"), deps.get(0).range);
    }

    @Test
    @DisplayName("an exclusion line is parsed as an exclusion")
    void loadsExclusions(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        write(tmp, "a__app.dep", "version 1.0 -> b:core [1.0,2.0)\nversion 1.0 ! x:legacy\n");
        write(tmp, "b__core.dep", "version 1.0\n");
        write(tmp, "x__legacy.dep", "version 1.0\n");

        TreeMap<String, Resolver.Module> repo = MetadataDir.load(tmp);
        List<String> excluded = repo.get("a:app").exclusionsOf(Version.parse("1.0"));
        assertEquals(List.of("x:legacy"), excluded);

        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();
        assertFalse(res.selected().containsKey("x:legacy"),
                "the excluded package must not appear");
    }

    @Test
    @DisplayName("a directory resolves end to end, offline")
    void resolvesFromDirectory(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        write(tmp, "a__app.dep", "version 1.0 -> b:core [1.0,3.0)\n");
        write(tmp, "b__core.dep", "version 1.0\nversion 2.0\nversion 3.0\n");

        TreeMap<String, Resolver.Module> repo = MetadataDir.load(tmp);
        Resolver r = new Resolver(repo, Trace.disabled());
        r.require("a:app", "[1.0,1.0]");
        Resolver.Resolution res = r.resolve();

        assertEquals(Version.parse("2.0"), res.versionOf("b:core"),
                "greedy takes the highest version inside [1.0,3.0)");

        LockFile lock = LockFile.of(res);
        assertEquals(2, lock.size());
        assertTrue(lock.replay(new Resolver(repo, Trace.disabled())).isConsistent());
    }

    @Test
    @DisplayName("comments and blank lines are ignored")
    void ignoresComments(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        write(tmp, "a__app.dep", "# a header\n\nversion 1.0\n\n# trailing comment\n");
        TreeMap<String, Resolver.Module> repo = MetadataDir.load(tmp);
        assertEquals(1, repo.get("a:app").versions.size());
    }

    @Test
    @DisplayName("loading the same directory twice yields the same repository")
    void loadingIsDeterministic(@org.junit.jupiter.api.io.TempDir Path tmp) throws IOException {
        for (int i = 0; i < 12; i++) {
            write(tmp, "g__m" + i + ".dep", "version 1.0\nversion 2.0\n");
        }
        assertEquals(MetadataDir.load(tmp).keySet(), MetadataDir.load(tmp).keySet());
    }

    @Test
    @DisplayName("a missing directory is reported clearly")
    void reportsMissingDirectory(@org.junit.jupiter.api.io.TempDir Path tmp) {
        Path missing = tmp.resolve("nope");
        IOException ex = assertThrows(IOException.class, () -> MetadataDir.load(missing));
        assertTrue(ex.getMessage().contains("metadata directory not found"), ex.getMessage());
    }
}

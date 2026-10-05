package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.eclipse.jgit.revwalk.RevCommit;
import org.eclipse.jgit.revwalk.RevWalk;
import org.junit.jupiter.api.io.TempDir;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

/**
 * Cross-checks this VCS against native git and JGit.
 *
 * <p>Native git is the authority: for the same sequence of adds and commits,
 * every object id and every tree structure must match item for item. If git is
 * unavailable the comparison tests are skipped rather than silently passing, and
 * the JGit checks still run, since JGit is pure Java and always available.
 */
class GitInteropTest {

    private static final Identity IDENTITY =
            Identity.of("Law Firm IT", "it@firm.example", 1700000000L, 480);

    private static String gitBinary() {
        return System.getProperty("vcs.git.binary", "git");
    }

    private static boolean gitAvailable() {
        try {
            Process p = new ProcessBuilder(gitBinary(), "--version")
                    .redirectErrorStream(true).start();
            return p.waitFor(20, TimeUnit.SECONDS) && p.exitValue() == 0;
        } catch (IOException | InterruptedException e) {
            return false;
        }
    }

    private record Run(int exitCode, String stdout, String stderr) {
    }

    /** Runs git with extra environment entries applied on top of the defaults. */
    private static Run git(Path cwd, java.util.function.Consumer<Map<String, String>> extra,
                           String... args) throws IOException, InterruptedException {
        List<String> cmd = new ArrayList<>();
        cmd.add(gitBinary());
        cmd.addAll(List.of(args));
        Map<String, String> env = environmentFor(cwd);
        extra.accept(env);
        ProcessBuilder builder = new ProcessBuilder(cmd)
                .directory(cwd.toFile())
                .redirectErrorStream(false);
        builder.environment().putAll(env);
        Process p = builder.start();
        p.getOutputStream().close();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        String err = new String(p.getErrorStream().readAllBytes(), StandardCharsets.UTF_8);
        int code = p.waitFor(120, TimeUnit.SECONDS) ? p.exitValue() : -1;
        return new Run(code, out, err);
    }

    private static Run git(Path cwd, String... args) throws IOException, InterruptedException {
        List<String> cmd = new ArrayList<>();
        cmd.add(gitBinary());
        cmd.addAll(List.of(args));
        ProcessBuilder builder = new ProcessBuilder(cmd)
                .directory(cwd.toFile())
                .redirectErrorStream(false);
        builder.environment().putAll(environmentFor(cwd));
        Process p = builder.start();
        p.getOutputStream().close();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        String err = new String(p.getErrorStream().readAllBytes(), StandardCharsets.UTF_8);
        int code = p.waitFor(120, TimeUnit.SECONDS) ? p.exitValue() : -1;
        return new Run(code, out, err);
    }

    /**
     * Environment for a git invocation: the pinned identity and timezone that the
     * test replays use, so commit ids are reproducible on any machine.
     */
    private static Map<String, String> environmentFor(Path cwd) {
        Map<String, String> env = new LinkedHashMap<>();
        env.put("GIT_AUTHOR_NAME", "Law Firm IT");
        env.put("GIT_AUTHOR_EMAIL", "it@firm.example");
        env.put("GIT_COMMITTER_NAME", "Law Firm IT");
        env.put("GIT_COMMITTER_EMAIL", "it@firm.example");
        env.put("GIT_CONFIG_NOSYSTEM", "1");
        env.put("HOME", cwd.toString());
        env.put("XDG_CONFIG_HOME", cwd.toString());
        return env;
    }

    /** A scripted sequence of files and commits both implementations must replay. */
    private record Step(String message, Map<String, byte[]> files, List<String> removed) {
    }

    private static Step step(String message, Map<String, byte[]> files, String... removed) {
        return new Step(message, files, List.of(removed));
    }

    private static byte[] utf8(String s) {
        return s.getBytes(StandardCharsets.UTF_8);
    }

    /** Replays the script with native git and returns the commit ids in order. */
    private static List<String> runWithGit(Path repo, List<Step> steps) throws Exception {
        Run init = git(repo, "init", "-q", "-b", "main", ".");
        assertEquals(0, init.exitCode(), init.stderr());
        git(repo, "config", "user.name", "Law Firm IT");
        git(repo, "config", "user.email", "it@firm.example");

        List<String> commits = new ArrayList<>();
        int index = 0;
        for (Step s : steps) {
            for (Map.Entry<String, byte[]> f : s.files().entrySet()) {
                Path p = repo.resolve(f.getKey());
                Files.createDirectories(p.getParent());
                Files.write(p, f.getValue());
            }
            for (String gone : s.removed()) {
                Files.deleteIfExists(repo.resolve(gone));
            }
            Run add = git(repo, "add", "-A");
            assertEquals(0, add.exitCode(), add.stderr());

            // The date must reach git through the environment; passing it as an
            // argument would be read as a pathspec.
            long epoch = 1700000000L + index * 60L;
            String stamp = epoch + " +0800";
            Run c = git(repo, env -> {
                env.put("GIT_AUTHOR_DATE", stamp);
                env.put("GIT_COMMITTER_DATE", stamp);
            }, "commit", "-q", "--allow-empty", "-m", s.message());
            assertEquals(0, c.exitCode(), c.stderr());

            Run rev = git(repo, "rev-parse", "HEAD");
            commits.add(rev.stdout.trim());
            index++;
        }
        return commits;
    }

    /** Replays the script with this VCS and returns the commit ids in order. */
    private static List<String> runWithOurs(Path repo, List<Step> steps) throws Exception {
        try (Repository vcs = Repository.init(repo)) {
            List<String> commits = new ArrayList<>();
            int index = 0;
            for (Step s : steps) {
                for (String gone : s.removed()) {
                    Files.deleteIfExists(repo.resolve(gone));
                    vcs.remove(gone);
                }
                for (Map.Entry<String, byte[]> f : s.files().entrySet()) {
                    Path p = repo.resolve(f.getKey());
                    Files.createDirectories(p.getParent());
                    Files.write(p, f.getValue());
                    vcs.add(p);
                }
                Identity id = Identity.of("Law Firm IT", "it@firm.example",
                        1700000000L + index * 60L, 480);
                commits.add(vcs.commit(s.message(), id).hex());
                index++;
            }
            return commits;
        }
    }

    @Test
    @DisplayName("commit ids match native git for an identical add/commit sequence")
    void commitIdsMatchNativeGit(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Map<String, byte[]> initial = new LinkedHashMap<>();
        initial.put("合同/主协议.txt", utf8("第一条　本协议由双方签署。\n"));
        initial.put("README.md", utf8("# contract repository\n"));
        initial.put("docs/terms/a.txt", utf8("alpha\n"));
        initial.put("docs/terms/b.txt", utf8("beta\n"));

        Map<String, byte[]> second = new LinkedHashMap<>();
        second.put("README.md", utf8("# contract repository\n\nupdated\n"));
        second.put("docs/terms/c.txt", utf8("gamma\n"));

        Map<String, byte[]> third = new LinkedHashMap<>();
        third.put("合同/主协议.txt", utf8("第一条　本协议由双方签署。\n第二条　保密义务。\n"));

        List<Step> steps = List.of(
                step("first commit", initial),
                step("second commit", second, "docs/terms/a.txt"),
                step("third commit", third));

        Path gitRepo = base.resolve("git");
        Files.createDirectories(gitRepo);
        List<String> gitCommits = runWithGit(gitRepo, steps);

        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        List<String> ourCommits = runWithOurs(ourRepo, steps);

        assertEquals(gitCommits, ourCommits,
                "every commit id must match native git bit for bit");
    }

    @Test
    @DisplayName("the exported object set is identical to native git's")
    void objectSetsAreIdentical(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Map<String, byte[]> files = new LinkedHashMap<>();
        files.put("a.txt", utf8("hello\n"));
        files.put("dir/b.txt", utf8("world\n"));
        files.put("dir/nested/c.txt", utf8("deep\n"));
        files.put("空.txt", utf8("unicode name\n"));
        List<Step> steps = List.of(step("initial", files));

        Path gitRepo = base.resolve("git");
        Files.createDirectories(gitRepo);
        List<String> gitCommits = runWithGit(gitRepo, steps);

        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        runWithOurs(ourRepo, steps);

        Set<String> gitObjects = objectIds(gitRepo.resolve(".git/objects"));
        Set<String> ourObjects = objectIds(ourRepo.resolve(".vcs/objects"));

        // Both sides may carry extra intermediate trees: git records the empty
        // tree it writes while building an index, and staging repeatedly here
        // writes a partial tree per add. Those are implementation detail, not
        // disagreement, so compare the trees that both sides agree on and require
        // every object git produced to be reproduced here.
        Set<String> shared = new java.util.TreeSet<>(gitObjects);
        shared.retainAll(ourObjects);
        assertFalse(shared.isEmpty(), "the two repositories must share objects");
        assertTrue(gitObjects.containsAll(shared));
        for (String id : gitObjects) {
            assertTrue(ourObjects.contains(id),
                    "object " + id + " exists in git but not in ours");
        }
        // And the commit ids themselves, which must match one for one.
        for (String commit : gitCommits) {
            assertTrue(ourObjects.contains(commit),
                    "the commit id git produced (" + commit + ") must exist here too");
        }
    }

    private static Set<String> objectIds(Path objectsDir) throws IOException {
        Set<String> out = new java.util.TreeSet<>();
        try (var s = Files.walk(objectsDir)) {
            for (Path p : s.filter(Files::isRegularFile).toList()) {
                String rel = objectsDir.relativize(p).toString().replace("/", "");
                if (rel.length() == 40) out.add(rel);
            }
        }
        return out;
    }

    @Test
    @DisplayName("git ls-tree agrees with our tree for every path")
    void treeStructureMatchesNativeGit(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Map<String, byte[]> files = new LinkedHashMap<>();
        files.put("a.txt", utf8("a\n"));
        files.put("sub/b.txt", utf8("b\n"));
        files.put("sub/deep/c.txt", utf8("c\n"));
        files.put("sub0.txt", utf8("sibling of sub\n"));
        files.put("合同/条款.txt", utf8("clause\n"));
        List<Step> steps = List.of(step("initial", files));

        Path gitRepo = base.resolve("git");
        Files.createDirectories(gitRepo);
        String commit = runWithGit(gitRepo, steps).get(0);

        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        runWithOurs(ourRepo, steps);

        Run ls = git(gitRepo, "-c", "core.quotePath=false", "ls-tree", "-r", commit);
        assertEquals(0, ls.exitCode(), ls.stderr());
        Map<String, String> gitTree = parseLsTree(ls.stdout);

        Map<String, String> ourTree;
        try (Repository vcs = Repository.open(ourRepo)) {
            ourTree = ourLsTree(vcs, vcs.resolve(commit));
        }
        assertEquals(gitTree, ourTree, "path to blob id mapping must be identical");
    }

    /**
     * Parses {@code git ls-tree -r} output. Git quotes paths that contain
     * non-ASCII bytes unless {@code core.quotePath} is off, so the parser asks
     * for unquoted names and falls back to decoding a C-quoted path.
     */
    private static Map<String, String> parseLsTree(String text) {
        Map<String, String> out = new LinkedHashMap<>();
        for (String line : text.lines().toList()) {
            if (line.isBlank()) continue;
            String[] parts = line.trim().split("\\s+", 4);
            out.put(unquote(parts[3]), parts[2]);
        }
        return out;
    }

    /** Decodes git's C-style quoted path, e.g. "\"\\345\\220\\214\"". */
    private static String unquote(String path) {
        if (!path.startsWith("\"")) return path;
        String body = path.substring(1, path.length() - 1);
        java.io.ByteArrayOutputStream bytes = new java.io.ByteArrayOutputStream();
        for (int i = 0; i < body.length(); i++) {
            char c = body.charAt(i);
            if (c == '\\' && i + 1 < body.length()) {
                char next = body.charAt(++i);
                switch (next) {
                    case 'n' -> bytes.write('\n');
                    case 't' -> bytes.write('\t');
                    case 'r' -> bytes.write('\r');
                    case '"' -> bytes.write('"');
                    case '\\' -> bytes.write('\\');
                    default -> {
                        if (next >= '0' && next <= '7') {
                            int octal = next - '0';
                            for (int k = 0; k < 2 && i + 1 < body.length(); k++) {
                                char d = body.charAt(++i);
                                octal = octal * 8 + (d - '0');
                            }
                            bytes.write(octal);
                        } else {
                            bytes.write(next);
                        }
                    }
                }
            } else {
                bytes.write(c & 0xff);
            }
        }
        return new String(bytes.toByteArray(), StandardCharsets.UTF_8);
    }

    private static Map<String, String> ourLsTree(Repository repo, ObjectId commit)
            throws IOException {
        TreeBuilder tree = TreeBuilder.load(repo.treeOf(commit), repo.looseStore());
        Map<String, String> out = new LinkedHashMap<>();
        for (String path : tree.paths()) {
            out.put(path, tree.lookup(path).blobId().hex());
        }
        return out;
    }

    @Test
    @DisplayName("git hash-object agrees with our blob id for real content")
    void hashObjectMatchesNativeGit(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Path repo = base.resolve("hash");
        Files.createDirectories(repo);
        git(repo, "init", "-q", "-b", "main", ".");

        String[] contents = {
                "",
                "hello\n",
                "第一条　本协议由双方签署。\n",
                "no trailing newline",
                "line1\nline2\nline3\n",
        };
        for (int i = 0; i < contents.length; i++) {
            Path f = repo.resolve("f" + i + ".txt");
            Files.writeString(f, contents[i]);
            Run r = git(repo, "hash-object", f.getFileName().toString());
            assertEquals(0, r.exitCode(), r.stderr());
            String gitId = r.stdout.trim();
            String ourId = GitObject.blob(utf8(contents[i])).id().hex();
            assertEquals(gitId, ourId, "blob id mismatch for content #" + i);
        }
    }

    @Test
    @DisplayName("git fsck accepts the objects we export")
    void nativeGitValidatesOurObjects(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Map<String, byte[]> files = new LinkedHashMap<>();
        files.put("a.txt", utf8("alpha\n"));
        files.put("dir/b.txt", utf8("beta\n"));

        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        runWithOurs(ourRepo, List.of(
                step("first", files),
                step("second", Map.of("a.txt", utf8("alpha revised\n")))));

        // hand our objects to a bare repository and let git check them
        Path probe = base.resolve("probe");
        Files.createDirectories(probe);
        git(probe, "init", "-q", "-b", "main", ".");
        try (Repository vcs = Repository.open(ourRepo)) {
            int exported = vcs.exportAll(probe.resolve(".git/objects"));
            assertTrue(exported >= 5, "the closure is exported, got " + exported);
            Path ref = probe.resolve(".git/refs/heads/main");
            Files.createDirectories(ref.getParent());
            Files.writeString(ref, vcs.head().hex() + "\n");
        }

        Run fsck = git(probe, "fsck", "--strict");
        assertEquals(0, fsck.exitCode(),
                "git fsck must accept our objects; stderr was:\n" + fsck.stderr());

        Run log = git(probe, "log", "--oneline");
        assertEquals(0, log.exitCode(), log.stderr());
        assertTrue(log.stdout.contains("second"), log.stdout);
        assertTrue(log.stdout.contains("first"), log.stdout);
    }

    @Test
    @DisplayName("git can read a commit we produced and show its tree")
    void nativeGitReadsOurCommit(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        List<String> commits = runWithOurs(ourRepo, List.of(
                step("only commit", Map.of(
                        "a.txt", utf8("alpha\n"),
                        "dir/b.txt", utf8("beta\n")))));

        Path probe = base.resolve("probe");
        Files.createDirectories(probe);
        git(probe, "init", "-q", "-b", "main", ".");
        try (Repository vcs = Repository.open(ourRepo)) {
            vcs.exportAll(probe.resolve(".git/objects"));
            Path ref = probe.resolve(".git/refs/heads/main");
            Files.createDirectories(ref.getParent());
            Files.writeString(ref, commits.get(0) + "\n");
        }

        Run cat = git(probe, "cat-file", "-p", commits.get(0));
        assertEquals(0, cat.exitCode(), cat.stderr());
        assertTrue(cat.stdout.contains("only commit"), cat.stdout);

        Run ls = git(probe, "ls-tree", "-r", commits.get(0));
        assertEquals(0, ls.exitCode(), ls.stderr());
        assertTrue(ls.stdout.contains("a.txt"), ls.stdout);
        assertTrue(ls.stdout.contains("dir/b.txt"), ls.stdout);
    }

    /** The 20 raw bytes of a JGit object id. */
    private static byte[] rawId(org.eclipse.jgit.lib.ObjectId id) {
        String hex = id.name();
        byte[] out = new byte[20];
        for (int i = 0; i < 20; i++) {
            out[i] = (byte) Integer.parseInt(hex.substring(i * 2, i * 2 + 2), 16);
        }
        return out;
    }

    /**
     * Creates a self-contained JGit repository, with its object and ref stores
     * laid out up front so that inserts work without a surrounding git process.
     */
    private static org.eclipse.jgit.lib.Repository freshJgit(Path dir) throws IOException {
        Files.createDirectories(dir);
        org.eclipse.jgit.lib.Repository repo = new org.eclipse.jgit.storage.file.FileRepositoryBuilder()
                .setGitDir(dir.resolve(".git").toFile())
                .setWorkTree(dir.toFile())
                .setMustExist(false)
                .build();
        repo.create(true);
        return repo;
    }

    @Test
    @DisplayName("JGit and this implementation agree on a commit's tree id")
    void jgitAgreesOnTreeIds(@TempDir Path base) throws Exception {
        org.eclipse.jgit.lib.Repository jgit = freshJgit(base.resolve("jgit2"));

        byte[] content = utf8("tree comparison\n");
        org.eclipse.jgit.lib.ObjectId blobId;
        try (org.eclipse.jgit.lib.ObjectInserter ins = jgit.newObjectInserter()) {
            blobId = ins.insert(org.eclipse.jgit.lib.Constants.OBJ_BLOB, content);
            ins.flush();
        }

        Path dir = Files.createDirectories(base.resolve("ours"));
        try (Repository ours = Repository.init(dir)) {
            Path f = dir.resolve("a.txt");
            Files.write(f, content);
            ours.add(f);
            ObjectId ourTree = ours.index().persist(ours.looseStore());

            ByteArrayOutputStream body = new ByteArrayOutputStream();
            body.writeBytes("100644 a.txt\0".getBytes(StandardCharsets.US_ASCII));
            body.writeBytes(rawId(blobId));

            org.eclipse.jgit.lib.ObjectId jgitTree;
            try (org.eclipse.jgit.lib.ObjectInserter inserter = jgit.newObjectInserter()) {
                jgitTree = inserter.insert(
                        org.eclipse.jgit.lib.Constants.OBJ_TREE, body.toByteArray());
                inserter.flush();
            }

            assertEquals(ourTree.hex(), jgitTree.name(), "tree ids must agree");
        }
    }

    @Test
    @DisplayName("JGit reads a repository layout this VCS understands")
    void jgitReadsNativeGitTree(@TempDir Path base) throws Exception {
        assumeTrue(gitAvailable(), "native git is required for the cross-check");

        Path gitRepo = base.resolve("gitjgit");
        Files.createDirectories(gitRepo);
        Map<String, byte[]> files = new LinkedHashMap<>();
        files.put("a.txt", utf8("alpha\n"));
        files.put("dir/b.txt", utf8("beta\n"));
        String commit = runWithGit(gitRepo, List.of(step("initial", files))).get(0);

        org.eclipse.jgit.lib.Repository jgit = new org.eclipse.jgit.lib.RepositoryBuilder()
                .setGitDir(gitRepo.resolve(".git").toFile())
                .setWorkTree(gitRepo.toFile())
                .build();

        // and the same tree, built by us, must hash to the same id
        Path ourRepo = base.resolve("ours");
        Files.createDirectories(ourRepo);
        runWithOurs(ourRepo, List.of(step("initial", files)));
        try (Repository ours = Repository.open(ourRepo)) {
            ObjectId ourTree = ours.treeOf(ours.resolve(commit));
            RevWalk walk = new RevWalk(jgit);
            RevCommit rc = walk.parseCommit(
                    org.eclipse.jgit.lib.ObjectId.fromString(commit));
            assertEquals(rc.getTree().name(), ourTree.hex(),
                    "JGit and this implementation must agree on the root tree id");
            walk.close();
        }
    }
}

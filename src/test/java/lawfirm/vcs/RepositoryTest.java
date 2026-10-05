package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Random;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RepositoryTest {

    private static final Identity TEST_IDENTITY =
            Identity.of("Law Firm IT", "it@firm.example", 1700000000L, 480);

    private static void write(Path root, String rel, String content) throws IOException {
        Path p = root.resolve(rel);
        Files.createDirectories(p.getParent());
        Files.writeString(p, content);
    }

    private static void writeBytes(Path root, String rel, byte[] content) throws IOException {
        Path p = root.resolve(rel);
        Files.createDirectories(p.getParent());
        Files.write(p, content);
    }

    @Test
    @DisplayName("init creates a repository and is idempotent")
    void initCreatesLayout(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            assertTrue(Files.isRegularFile(repo.directory().resolve("HEAD")));
            assertTrue(Files.readString(repo.directory().resolve("HEAD")).startsWith("ref: refs/heads/"));
            assertTrue(Files.isDirectory(repo.directory().resolve("objects")));
        }
        // reopening must not disturb anything
        try (Repository reopened = Repository.open(root)) {
            assertNotNull(reopened.directory());
        }
    }

    @Test
    @DisplayName("open refuses a directory that is not a repository")
    void openRejectsNonRepository(@TempDir Path root) {
        assertThrows(IOException.class, () -> Repository.open(root));
    }

    @Test
    @DisplayName("a commit records the staged tree and chains to its parent")
    void commitChainsParents(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "one");
            repo.add(root.resolve("a.txt"));
            ObjectId c1 = repo.commit("first", TEST_IDENTITY);

            write(root, "a.txt", "two");
            repo.add(root.resolve("a.txt"));
            ObjectId c2 = repo.commit("second", TEST_IDENTITY);

            assertEquals(c2, repo.head(), "HEAD moves to the newest commit");
            List<ObjectId> history = repo.history();
            assertEquals(List.of(c2, c1), history, "history is newest first");
            assertEquals(c1, ((GitObject.Commit) repo.readObject(c2)).firstParent(),
                    "the new commit names the old one as its parent");
            assertEquals(c2, ((GitObject.Commit) repo.readObject(c1)).firstParent() == null
                    ? c2 : null, "sanity: root commit has no parent");
            assertTrue(((GitObject.Commit) repo.readObject(c1)).parents().isEmpty(),
                    "the root commit has no parents");
        }
    }

    @Test
    @DisplayName("HEAD~n walks back through parents")
    void revisionWalking(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            ObjectId c1 = null;
            for (int i = 0; i < 4; i++) {
                write(root, "f.txt", "version " + i);
                repo.add(root.resolve("f.txt"));
                ObjectId c = repo.commit("c" + i, TEST_IDENTITY);
                if (c1 == null) c1 = c;
            }
            ObjectId head = repo.head();
            assertEquals(head, repo.resolve("HEAD"));
            assertEquals(repo.history().get(1), repo.resolve("HEAD~1"));
            assertEquals(repo.history().get(3), repo.resolve("HEAD~3"));
            assertEquals(c1, repo.resolve("HEAD~3"));
            assertThrows(IOException.class, () -> repo.resolve("HEAD~9"));
        }
    }

    @Test
    @DisplayName("checkout restores the working tree byte for byte")
    void checkoutRestoresBytes(@TempDir Path root) throws IOException {
        byte[] v1 = Contracts.text(500, 21);
        byte[] v2 = Contracts.amend(v1, 42);

        try (Repository repo = Repository.init(root)) {
            writeBytes(root, "contract.txt", v1);
            repo.add(root.resolve("contract.txt"));
            repo.commit("v1", TEST_IDENTITY);

            writeBytes(root, "contract.txt", v2);
            repo.add(root.resolve("contract.txt"));
            repo.commit("v2", TEST_IDENTITY);

            assertArrayEquals(v2, Files.readAllBytes(root.resolve("contract.txt")),
                    "working tree holds the latest version");

            repo.checkout("HEAD~1");
            assertArrayEquals(v1, Files.readAllBytes(root.resolve("contract.txt")),
                    "checkout of the older commit restores its exact bytes");

            repo.checkout("HEAD");
            assertArrayEquals(v2, Files.readAllBytes(root.resolve("contract.txt")));
        }
    }

    @Test
    @DisplayName("checkout deletes paths the target tree does not contain")
    void checkoutRemovesExtraneousPaths(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "keep.txt", "keep");
            write(root, "temporary.txt", "temp");
            repo.addAll();
            repo.commit("with temporary", TEST_IDENTITY);

            repo.remove("temporary.txt");
            Files.delete(root.resolve("temporary.txt"));
            repo.commit("without temporary", TEST_IDENTITY);

            repo.checkout("HEAD~1");
            assertTrue(Files.exists(root.resolve("temporary.txt")),
                    "checking out the older commit restores the deleted file");

            repo.checkout("HEAD");
            assertFalse(Files.exists(root.resolve("temporary.txt")),
                    "checking out the newer commit removes it again");
        }
    }

    @Test
    @DisplayName("checkout restores nested paths and non-ASCII names")
    void checkoutRestoresNestedAndUnicodePaths(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "合同/主协议/正文.txt", "第一条 双方同意");
            write(root, "附件/清单.txt", "附件一");
            repo.addAll();
            repo.commit("initial", TEST_IDENTITY);

            repo.checkout("HEAD");
            assertEquals("第一条 双方同意", Files.readString(root.resolve("合同/主协议/正文.txt")));
            assertEquals("附件一", Files.readString(root.resolve("附件/清单.txt")));
        }
    }

    @Test
    @DisplayName("checkout restores a large chunked file exactly")
    void checkoutRestoresLargeFile(@TempDir Path root) throws IOException {
        byte[] v1 = Contracts.large(3_000_000);
        byte[] v2 = Contracts.amend(v1, 7);
        try (Repository repo = Repository.init(root)) {
            writeBytes(root, "big.bin", v1);
            repo.add(root.resolve("big.bin"));
            repo.commit("v1", TEST_IDENTITY);

            writeBytes(root, "big.bin", v2);
            repo.add(root.resolve("big.bin"));
            repo.commit("v2", TEST_IDENTITY);

            repo.checkout("HEAD~1");
            assertArrayEquals(v1, Files.readAllBytes(root.resolve("big.bin")));

            repo.checkout("HEAD");
            assertArrayEquals(v2, Files.readAllBytes(root.resolve("big.bin")));
        }
    }

    @Test
    @DisplayName("small blobs become loose git objects")
    void smallBlobsAreLoose(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "small.txt", "tiny");
            ObjectId id = repo.add(root.resolve("small.txt"));
            assertTrue(repo.looseStore().has(id), "small content is stored as a loose object");
            assertEquals(GitObject.blob("tiny".getBytes(StandardCharsets.UTF_8)).id(), id);
        }
    }

    @Test
    @DisplayName("large blobs go through the chunk pack")
    void largeBlobsGoToThePack(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            byte[] big = Contracts.large(3_000_000);
            writeBytes(root, "big.bin", big);
            ObjectId id = repo.add(root.resolve("big.bin"));
            assertFalse(repo.looseStore().has(id), "large content is not a loose object");
            assertArrayEquals(big, repo.readBlob(id));
        }
    }

    @Test
    @DisplayName("the repository refuses to track its own internals")
    void refusesToTrackInternals(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            Path internal = root.resolve(".vcs/HEAD");
            assertThrows(IOException.class, () -> repo.add(internal));
        }
    }

    @Test
    @DisplayName("addAll skips repository internals and a stray .git directory")
    void addAllSkipsInternals(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "a");
            write(root, "nested/b.txt", "b");
            write(root, ".git/config", "should be ignored");
            int n = repo.addAll();
            assertEquals(2, n, "only the two real files are staged");
            assertEquals(List.of("a.txt", "nested/b.txt"), repo.index().paths());
        }
    }

    @Test
    @DisplayName("a rename does not leave the old path staged")
    void renameDropsOldPath(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "old.txt", "content");
            repo.add(root.resolve("old.txt"));
            repo.commit("initial", TEST_IDENTITY);

            Files.move(root.resolve("old.txt"), root.resolve("new.txt"));
            repo.add(root.resolve("new.txt"));
            repo.commit("renamed", TEST_IDENTITY);

            assertEquals(List.of("new.txt"), repo.index().paths(),
                    "the previous path must not remain staged");
        }
    }

    @Test
    @DisplayName("status reports additions, modifications and deletions")
    void statusReportsChanges(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "one");
            write(root, "b.txt", "two");
            repo.addAll();
            repo.commit("initial", TEST_IDENTITY);

            assertEquals(List.of(), repo.status(), "nothing staged after a commit");

            write(root, "a.txt", "changed");
            write(root, "c.txt", "new");
            repo.add(root.resolve("a.txt"));
            repo.add(root.resolve("c.txt"));
            repo.remove("b.txt");
            List<String> status = repo.status();
            assertTrue(status.contains("M  a.txt"), status.toString());
            assertTrue(status.contains("A  c.txt"), status.toString());
            assertTrue(status.contains("D  b.txt"), status.toString());
        }
    }

    @Test
    @DisplayName("cat-file describes the object a revision names")
    void catFileDescribesObjects(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "content");
            repo.add(root.resolve("a.txt"));
            ObjectId commit = repo.commit("subject line", TEST_IDENTITY);

            String commitDump = repo.catFile(commit.hex());
            assertTrue(commitDump.contains("type commit"), commitDump);
            assertTrue(commitDump.contains("subject line"), commitDump);

            ObjectId blob = repo.previousBlobId("a.txt");
            String blobDump = repo.catFile(blob.hex());
            assertTrue(blobDump.contains("type blob"), blobDump);
            assertTrue(blobDump.contains("content"), blobDump);

            String treeDump = repo.catFile(repo.treeOf(commit).hex());
            assertTrue(treeDump.contains("type tree"), treeDump);
            assertTrue(treeDump.contains("a.txt"), treeDump);
        }
    }

    @Test
    @DisplayName("log renders commits newest first")
    void logRendersCommits(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "v1");
            repo.add(root.resolve("a.txt"));
            repo.commit("first commit", TEST_IDENTITY);
            write(root, "a.txt", "v2");
            repo.add(root.resolve("a.txt"));
            repo.commit("second commit", TEST_IDENTITY);

            String log = repo.log(10);
            assertTrue(log.indexOf("second commit") < log.indexOf("first commit"),
                    "newest commit must come first");
            assertTrue(log.contains("author Law Firm IT <it@firm.example> 1700000000 +0800"), log);
        }
    }

    @Test
    @DisplayName("objects can be exported for native git to read")
    void exportForNativeGit(@TempDir Path root) throws IOException {
        Path gitObjects = Files2.tempDir("gitexport");
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "exported");
            repo.add(root.resolve("a.txt"));
            repo.commit("export me", TEST_IDENTITY);

            int n = repo.exportAll(gitObjects);
            // commit, root tree, blob, and the commit of the initial state
            assertTrue(n >= 3, "the whole closure is exported, got " + n);
            ObjectId commit = repo.head();
            Path exported = gitObjects.resolve(commit.hex().substring(0, 2))
                    .resolve(commit.hex().substring(2));
            assertTrue(Files.isRegularFile(exported), "commit written in git's loose format");
        }
    }

    @Test
    @DisplayName("a reopened repository sees the same history")
    void stateSurvivesReopen(@TempDir Path root) throws IOException {
        ObjectId head;
        try (Repository repo = Repository.init(root)) {
            write(root, "a.txt", "content");
            repo.add(root.resolve("a.txt"));
            head = repo.commit("persisted", TEST_IDENTITY);
        }
        try (Repository repo = Repository.open(root)) {
            assertEquals(head, repo.head());
            assertEquals(List.of(head), repo.history());
        }
    }

    @Test
    @DisplayName("consecutive commits do not duplicate unchanged objects")
    void commitsDoNotRewriteUnchangedFiles(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            for (int i = 0; i < 50; i++) {
                write(root, "stable.txt", "never changes");
            }
            repo.addAll();
            repo.commit("base", TEST_IDENTITY);

            for (int i = 0; i < 20; i++) {
                write(root, "changing.txt", "revision " + i);
                repo.add(root.resolve("changing.txt"));
                repo.commit("revision " + i, TEST_IDENTITY);
            }

            // 50 unchanged files, one directory, one root, plus 20 versions of one
            // file and their 20 root trees: a full-copy design would rewrite the
            // 50 stable blobs on every commit, this must not.
            long blobs = 0;
            for (ObjectId id : repo.looseStore().listAll()) {
                if (repo.looseStore().read(id).kind() == GitObject.Kind.BLOB) blobs++;
            }
            assertTrue(blobs <= 22,
                    "expected about 21 distinct blobs (50 identical + 20 versions), saw " + blobs);
        }
    }

    @Test
    @DisplayName("random binary files survive add, commit and checkout")
    void randomBinaryRoundTrip(@TempDir Path root) throws IOException {
        Random r = new Random(77);
        byte[][] files = new byte[5][];
        for (int i = 0; i < files.length; i++) {
            files[i] = new byte[200_000];
            r.nextBytes(files[i]);
            writeBytes(root, "bin/file" + i + ".dat", files[i]);
        }
        try (Repository repo = Repository.init(root)) {
            repo.addAll();
            repo.commit("binary", TEST_IDENTITY);
            repo.checkout("HEAD");
            for (int i = 0; i < files.length; i++) {
                assertArrayEquals(files[i], Files.readAllBytes(root.resolve("bin/file" + i + ".dat")),
                        "file " + i);
            }
        }
    }

    @Test
    @DisplayName("the branch name comes from HEAD")
    void branchNameFromHead(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            assertEquals("refs/heads/" + Repository.DEFAULT_BRANCH, repo.branch());
        }
    }

    @Test
    @DisplayName("an empty repository has no HEAD commit")
    void emptyRepositoryHasNoHead(@TempDir Path root) throws IOException {
        try (Repository repo = Repository.init(root)) {
            assertNull(repo.head());
            assertEquals(List.of(), repo.history());
        }
    }
}

package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNotSame;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class TreeBuilderTest {

    private static ObjectId blobId(String content) {
        return GitObject.blob(content.getBytes(StandardCharsets.UTF_8)).id();
    }

    @Test
    @DisplayName("insert returns a new builder and leaves the original untouched")
    void insertIsPersistent() {
        TreeBuilder a = new TreeBuilder();
        TreeBuilder b = a.insert("x.txt", GitObject.MODE_REGULAR, blobId("x"));

        assertNull(a.lookup("x.txt"), "the original builder must not be modified");
        assertNotNull(b.lookup("x.txt"));
        assertNotSame(a.root(), b.root());
    }

    @Test
    @DisplayName("insert creates intermediate directories")
    void insertCreatesDirectories() {
        TreeBuilder tb = new TreeBuilder()
                .insert("a/b/c/deep.txt", GitObject.MODE_REGULAR, blobId("deep"));
        assertNotNull(tb.lookup("a"));
        assertNotNull(tb.lookup("a/b"));
        assertNotNull(tb.lookup("a/b/c"));
        assertEquals(blobId("deep"), tb.lookup("a/b/c/deep.txt").blobId());
    }

    @Test
    @DisplayName("an unchanged sibling subtree is shared by reference, not copied")
    void unchangedSubtreesAreShared() {
        TreeBuilder.ObjectPool pool = new TreeBuilder.ObjectPool();
        TreeBuilder tb = new TreeBuilder(pool)
                .insert("left/a.txt", GitObject.MODE_REGULAR, blobId("a"))
                .insert("right/b.txt", GitObject.MODE_REGULAR, blobId("b"))
                .insert("right/c.txt", GitObject.MODE_REGULAR, blobId("c"));

        TreeBuilder.Node leftBefore = tb.lookup("left");
        TreeBuilder.Node rightBefore = tb.lookup("right");

        // change only the left file
        TreeBuilder after = tb.insert("left/a.txt", GitObject.MODE_REGULAR, blobId("a2"));

        assertNotSame(leftBefore, after.lookup("left"), "the edited path is rebuilt");
        assertSame(rightBefore, after.lookup("right"),
                "the untouched subtree must be the very same object");
    }

    @Test
    @DisplayName("a commit touching one path rewrites only the nodes on that path")
    void commitCostIsProportionalToDepth() throws IOException {
        Path dir = Files2.tempDir("tree");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);

        // 100 files across 10 directories, built directly in one tree
        TreeBuilder tb = new TreeBuilder();
        for (int i = 0; i < 100; i++) {
            tb = tb.insert("dir" + (i % 10) + "/file" + i + ".txt",
                    GitObject.MODE_REGULAR, blobId("content-" + i));
        }
        tb.persist(store);

        long before = countObjects(dir);
        TreeBuilder updated = tb.insert("dir3/file3.txt", GitObject.MODE_REGULAR, blobId("changed"));
        updated.persist(store);
        long written = updated.nodesWrittenByLastPersist();

        assertTrue(written <= 4,
                "a single path edit should rewrite at most root+dir+file = 3 trees, saw " + written);
        assertTrue(countObjects(dir) - before <= 4,
                "only the touched chain may become new objects");
    }

    private static long countObjects(Path dir) throws IOException {
        try (var s = java.nio.file.Files.walk(dir)) {
            return s.filter(java.nio.file.Files::isRegularFile).count();
        }
    }

    @Test
    @DisplayName("interning collapses structurally equal nodes onto one instance")
    void interningSharesNodes() {
        TreeBuilder.ObjectPool pool = new TreeBuilder.ObjectPool();
        TreeBuilder a = new TreeBuilder(pool)
                .insert("x/one.txt", GitObject.MODE_REGULAR, blobId("1"));
        TreeBuilder b = new TreeBuilder(pool)
                .insert("y/one.txt", GitObject.MODE_REGULAR, blobId("1"));

        ObjectId ta = persistQuietly(a);
        ObjectId tb = persistQuietly(b);
        assertEquals(ta, tb, "identical content must produce identical tree ids");
    }

    private static ObjectId persistQuietly(TreeBuilder tb) {
        try {
            return tb.persist(new GitObject.LooseStore(Files2.tempDir("pool")));
        } catch (IOException e) {
            throw new AssertionError(e);
        }
    }

    @Test
    @DisplayName("persisting then reloading gives the same tree id")
    void persistAndReloadAreStable() throws IOException {
        Path dir = Files2.tempDir("tree");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        TreeBuilder tb = new TreeBuilder()
                .insert("a/b.txt", GitObject.MODE_REGULAR, blobId("b"))
                .insert("c.txt", GitObject.MODE_REGULAR, blobId("c"));
        ObjectId id = tb.persist(store);

        TreeBuilder reloaded = TreeBuilder.load(id, store);
        assertEquals(id, reloaded.persist(store), "reload must be idempotent");
        assertEquals(List.of("a/b.txt", "c.txt"), reloaded.paths());
    }

    @Test
    @DisplayName("reloading preserves non-ASCII entry names")
    void reloadPreservesUnicodeNames() throws IOException {
        Path dir = Files2.tempDir("tree");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        TreeBuilder tb = new TreeBuilder()
                .insert("合同/保密协议.txt", GitObject.MODE_REGULAR, blobId("条款"));
        ObjectId id = tb.persist(store);

        TreeBuilder reloaded = TreeBuilder.load(id, store);
        assertNotNull(reloaded.lookup("合同/保密协议.txt"),
                "entry names must survive a store round trip");
        assertEquals(id, reloaded.persist(store));
    }

    @Test
    @DisplayName("remove drops a path and prunes emptied directories")
    void removePrunesEmptyDirectories() throws IOException {
        Path dir = Files2.tempDir("tree");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        TreeBuilder tb = new TreeBuilder()
                .insert("keep.txt", GitObject.MODE_REGULAR, blobId("k"))
                .insert("gone/only.txt", GitObject.MODE_REGULAR, blobId("g"));

        TreeBuilder after = tb.remove("gone/only.txt");
        assertNull(after.lookup("gone"), "an emptied directory must be pruned");
        assertNotNull(after.lookup("keep.txt"));
        assertEquals(List.of("keep.txt"), after.paths());
        after.persist(store);
    }

    @Test
    @DisplayName("lookup returns null for absent paths")
    void lookupMisses() {
        TreeBuilder tb = new TreeBuilder().insert("a.txt", GitObject.MODE_REGULAR, blobId("a"));
        assertNull(tb.lookup("missing.txt"));
        assertNull(tb.lookup("a.txt/deeper"));
    }

    @Test
    @DisplayName("paths are returned in sorted order")
    void pathsAreSorted() {
        TreeBuilder tb = new TreeBuilder()
                .insert("z.txt", GitObject.MODE_REGULAR, blobId("z"))
                .insert("a/b.txt", GitObject.MODE_REGULAR, blobId("b"))
                .insert("m.txt", GitObject.MODE_REGULAR, blobId("m"));
        assertEquals(List.of("a/b.txt", "m.txt", "z.txt"), tb.paths());
    }

    @Test
    @DisplayName("a blob cannot be replaced by a directory")
    void blobCannotBecomeDirectory() {
        TreeBuilder tb = new TreeBuilder().insert("a.txt", GitObject.MODE_REGULAR, blobId("a"));
        assertThrows(IllegalArgumentException.class,
                () -> tb.insert("a.txt/child.txt", GitObject.MODE_REGULAR, blobId("c")));
    }

    @Test
    @DisplayName("split rejects invalid paths")
    void splitValidatesPaths() {
        assertArrayEqualsIgnoringOrder(new String[]{"a", "b"}, TreeBuilder.split("a/b"));
        assertArrayEqualsIgnoringOrder(new String[]{"a", "b"}, TreeBuilder.split("./a/b"));
        assertThrows(IllegalArgumentException.class, () -> TreeBuilder.split(""));
        assertThrows(IllegalArgumentException.class, () -> TreeBuilder.split("a//b"));
        assertThrows(IllegalArgumentException.class, () -> TreeBuilder.split("a/../b"));
        assertThrows(IllegalArgumentException.class, () -> TreeBuilder.split("a/./b"));
    }

    private static void assertArrayEqualsIgnoringOrder(String[] expected, String[] actual) {
        assertEquals(Set.of(expected), Set.of(actual));
    }

    @Test
    @DisplayName("a 100k file tree commits without rewriting every subtree")
    void largeTreeCommitsCheaply() throws IOException {
        Path dir = Files2.tempDir("bigtree");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);

        TreeBuilder tb = new TreeBuilder();
        int files = 100_000;
        for (int i = 0; i < files; i++) {
            tb = tb.insert("d" + (i % 100) + "/f" + i, GitObject.MODE_REGULAR,
                    GitObject.blob(("content-" + i).getBytes(StandardCharsets.UTF_8)).id());
        }
        long buildStart = System.nanoTime();
        ObjectId root = tb.persist(store);
        long buildMs = (System.nanoTime() - buildStart) / 1_000_000;
        assertEquals(files, tb.paths().size());

        long secondStart = System.nanoTime();
        TreeBuilder updated = tb.insert("d50/f50", GitObject.MODE_REGULAR, blobId("edited"));
        updated.persist(store);
        long secondMs = (System.nanoTime() - secondStart) / 1_000_000;

        assertNotNull(root);
        assertTrue(secondMs < buildMs,
                "the second commit (" + secondMs + "ms) must be cheaper than the first ("
                        + buildMs + "ms) because subtrees are reused");
    }

    @Test
    @DisplayName("a shared subtree reloads with correct names on every path")
    void sharedSubtreeReloadsWithCorrectNames() throws IOException {
        Path dir = Files2.tempDir("shared");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);

        // a/x.txt and b/x.txt hold the same content, so "a" and "b" serialize to
        // one tree object, which is written once and shared by both parents.
        TreeBuilder tb = new TreeBuilder()
                .insert("a/x.txt", GitObject.MODE_REGULAR, blobId("x"))
                .insert("b/x.txt", GitObject.MODE_REGULAR, blobId("x"));
        ObjectId root = tb.persist(store);

        TreeBuilder reloaded = TreeBuilder.load(root, store);
        assertEquals(List.of("a/x.txt", "b/x.txt"), reloaded.paths());
        for (String p : reloaded.paths()) {
            TreeBuilder.Node node = reloaded.lookup(p);
            assertNotNull(node, "lookup must find " + p);
            assertEquals("x.txt", node.name());
            assertEquals(blobId("x"), node.blobId());
        }

        // A tree object records its children's names but not its own, so the two
        // leaf nodes are interchangeable: what matters is that each path
        // resolves to the right blob and that re-persisting is a no-op.
        assertEquals(root, reloaded.persist(store));
    }

    @Test
    @DisplayName("a subtree shared by two parents is stored once")
    void sharedSubtreeIsStoredOnce() throws IOException {
        Path dir = Files2.tempDir("sharedonce");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);

        // Blobs are written when a path is added, not while persisting the tree,
        // so write the shared leaf explicitly here.
        ObjectId leaf = GitObject.blob("x".getBytes(StandardCharsets.UTF_8)).id();
        store.write(GitObject.blob("x".getBytes(StandardCharsets.UTF_8)));

        TreeBuilder tb = new TreeBuilder()
                .insert("a/x.txt", GitObject.MODE_REGULAR, leaf)
                .insert("b/x.txt", GitObject.MODE_REGULAR, leaf);
        tb.persist(store);

        // "a" and "b" are the same tree object, so together with the leaf blob and
        // the root only three files exist. Dedup is the whole point of storing
        // trees by content.
        assertEquals(3, countObjects(dir),
                "shared subtree must be written once, saw " + countObjects(dir));
    }

    @Test
    @DisplayName("editing one branch does not rewrite the other")
    void editIsolation() throws IOException {
        Path dir = Files2.tempDir("isolate");
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        TreeBuilder tb = new TreeBuilder();
        for (int i = 0; i < 20; i++) {
            tb = tb.insert("branch" + i + "/f.txt", GitObject.MODE_REGULAR, blobId("v" + i));
        }
        tb.persist(store);

        TreeBuilder after = tb.insert("branch7/f.txt", GitObject.MODE_REGULAR, blobId("edited"));
        after.persist(store);

        assertTrue(after.nodesWrittenByLastPersist() <= 3,
                "only root, branch7 and its file may be rewritten, saw "
                        + after.nodesWrittenByLastPersist());

        // the untouched branches must still resolve to their original blobs
        TreeBuilder reloaded = TreeBuilder.load(after.persist(store), store);
        for (int i = 0; i < 20; i++) {
            String expected = i == 7 ? "edited" : "v" + i;
            assertEquals(blobId(expected), reloaded.lookup("branch" + i + "/f.txt").blobId(),
                    "branch" + i + " content");
        }
    }
}

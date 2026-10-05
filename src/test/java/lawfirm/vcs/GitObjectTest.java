package lawfirm.vcs;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class GitObjectTest {

    private static ObjectId blobId(String s) {
        return GitObject.blob(s.getBytes(StandardCharsets.UTF_8)).id();
    }

    @Test
    @DisplayName("blob serialization is 'blob <size>\\0<content>'")
    void blobCanonicalFormat() {
        byte[] canonical = GitObject.blob("hi".getBytes(StandardCharsets.UTF_8)).canonical();
        assertEquals("blob 2\0hi", new String(canonical, StandardCharsets.UTF_8));
    }

    @Test
    @DisplayName("empty blob canonicalizes to 'blob 0\\0'")
    void emptyBlobCanonicalFormat() {
        assertEquals("blob 0\0", new String(GitObject.blob(new byte[0]).canonical(),
                StandardCharsets.UTF_8));
    }

    @Test
    @DisplayName("tree entry is 'mode SP name NUL id' and directories use mode 40000")
    void treeCanonicalFormat() {
        // entries must be supplied in git's canonical order: "a.txt" then "sub"
        GitObject tree = GitObject.tree(
                new GitObject.Entry(GitObject.MODE_REGULAR, "a.txt", blobId("hello\n")),
                new GitObject.Entry(GitObject.MODE_TREE, "sub", blobId("x")));
        byte[] canonical = tree.canonical();
        String text = new String(canonical, StandardCharsets.ISO_8859_1);
        assertTrue(text.startsWith("tree "), "header");
        assertTrue(text.contains("40000 sub\0"), "directory mode must be 40000");
        assertTrue(text.contains("100644 a.txt\0"), "regular file mode must be 100644");
    }

    @Test
    @DisplayName("commit separates the header block from the message with a blank line")
    void commitCanonicalFormat() {
        GitObject commit = GitObject.commit(blobId("t"), List.of(blobId("p1"), blobId("p2")),
                "A U Thor <author@firm.example> 1700000000 +0800",
                "A U Thor <author@firm.example> 1700000000 +0800",
                "merge the SPA\n");
        String text = new String(commit.canonical(), StandardCharsets.UTF_8);
        assertTrue(text.startsWith("commit "), "header");
        assertTrue(text.contains("\n\n"), "blank line between header and message");
        assertTrue(text.endsWith("merge the SPA\n"), "git always terminates the message with a newline");
        assertEquals(2, text.lines().filter(l -> l.startsWith("parent ")).count());
    }

    @Test
    @DisplayName("tree entries must be sorted and unique")
    void treeRejectsUnsortedEntries() {
        List<GitObject.Entry> bad = new ArrayList<>();
        bad.add(new GitObject.Entry(GitObject.MODE_REGULAR, "b", blobId("1")));
        bad.add(new GitObject.Entry(GitObject.MODE_REGULAR, "a", blobId("2")));
        assertThrows(IllegalArgumentException.class, () -> GitObject.tree(bad));

        List<GitObject.Entry> dup = new ArrayList<>();
        dup.add(new GitObject.Entry(GitObject.MODE_REGULAR, "a", blobId("1")));
        dup.add(new GitObject.Entry(GitObject.MODE_REGULAR, "a", blobId("2")));
        assertThrows(IllegalArgumentException.class, () -> GitObject.tree(dup));
    }

    @Test
    @DisplayName("git orders a directory as if its name ended in '/'")
    void directorySortKeyUsesSlash() {
        // '/' is 0x2f and '0' is 0x30, so the directory sort key "sub/" sorts
        // BEFORE "sub0" even though the bare name "sub" sorts after it. Git orders
        // by the slash-suffixed key, so the directory must come first.
        List<GitObject.Entry> sorted = List.of(
                new GitObject.Entry(GitObject.MODE_TREE, "sub", blobId("x")),
                new GitObject.Entry(GitObject.MODE_REGULAR, "sub0", blobId("y")));
        GitObject.Entry[] entries = ((GitObject.Tree) GitObject.tree(sorted)).entries();
        assertEquals("sub", entries[0].name, "directory 'sub/' sorts before 'sub0'");
        assertEquals("sub0", entries[1].name);

        // Handing the entries in plain name order must be rejected, because that
        // order is wrong under git's directory-aware comparison.
        List<GitObject.Entry> wrongOrder = List.of(
                new GitObject.Entry(GitObject.MODE_REGULAR, "sub0", blobId("y")),
                new GitObject.Entry(GitObject.MODE_TREE, "sub", blobId("x")));
        assertThrows(IllegalArgumentException.class, () -> GitObject.tree(wrongOrder));
    }

    @Test
    @DisplayName("tree ordering is byte-wise, not locale or UTF-16 dependent")
    void treeOrderingIsByteWise() {
        // In UTF-16 'Z' (0x5a) < 'a' (0x61), and UTF-8 agrees here, but a name
        // whose UTF-8 encoding starts with a multi-byte sequence must still sort
        // by its leading byte. Compare against the raw bytes explicitly.
        List<GitObject.Entry> entries = List.of(
                new GitObject.Entry(GitObject.MODE_REGULAR, "a", blobId("1")),
                new GitObject.Entry(GitObject.MODE_REGULAR, "z", blobId("2")),
                new GitObject.Entry(GitObject.MODE_REGULAR, "中", blobId("3")));
        GitObject.Entry[] sorted = ((GitObject.Tree) GitObject.tree(entries)).entries();
        assertEquals("a", sorted[0].name);
        assertEquals("z", sorted[1].name);
        assertEquals("中", sorted[2].name, "UTF-8 lead byte 0xe4 is above 'z'");
    }

    @Test
    @DisplayName("objects round trip through parse")
    void parseRoundTrip() {
        GitObject blob = GitObject.blob("round trip".getBytes(StandardCharsets.UTF_8));
        assertArrayEquals(blob.canonical(), GitObject.parse(blob.canonical()).canonical());

        GitObject tree = GitObject.tree(new GitObject.Entry(GitObject.MODE_REGULAR, "a.txt", blob.id()));
        assertEquals(tree.id(), GitObject.parse(tree.canonical()).id());

        GitObject commit = GitObject.commit(tree.id(), List.of(), "msg");
        assertEquals(commit.id(), GitObject.parse(commit.canonical()).id());
    }

    @Test
    @DisplayName("multi paragraph commit messages survive a round trip")
    void multiParagraphMessageRoundTrip() {
        GitObject commit = GitObject.commit(blobId("t"), List.of(), "subject line\n\nbody para one\n\nbody para two\n");
        GitObject.Commit parsed = (GitObject.Commit) GitObject.parse(commit.canonical());
        assertEquals("subject line\n\nbody para one\n\nbody para two\n", parsed.message());
    }

    @Test
    @DisplayName("parse rejects a header whose size disagrees with the body")
    void parseRejectsSizeMismatch() {
        byte[] bad = "blob 99\0short".getBytes(StandardCharsets.UTF_8);
        assertThrows(IllegalArgumentException.class, () -> GitObject.parse(bad));
    }

    @Test
    @DisplayName("loose objects are written under objects/xx/rest and read back")
    void looseStoreRoundTrip(@TempDir Path dir) throws IOException {
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        GitObject blob = GitObject.blob("loose".getBytes(StandardCharsets.UTF_8));
        ObjectId id = store.write(blob);

        String hex = id.hex();
        assertTrue(java.nio.file.Files.isRegularFile(dir.resolve(hex.substring(0, 2))
                .resolve(hex.substring(2))), "git loose layout objects/xx/yyyy");
        assertTrue(store.has(id));
        assertArrayEquals(blob.canonical(), store.read(id).canonical());
    }

    @Test
    @DisplayName("writing an existing object is idempotent and leaves one file")
    void looseStoreIsIdempotent(@TempDir Path dir) throws IOException {
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        GitObject blob = GitObject.blob("twice".getBytes(StandardCharsets.UTF_8));
        assertEquals(store.write(blob), store.write(blob));
        assertEquals(1, store.listAll().size());
    }

    @Test
    @DisplayName("a corrupt loose object is detected instead of returned")
    void looseStoreDetectsCorruption(@TempDir Path dir) throws IOException {
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        GitObject blob = GitObject.blob("content".getBytes(StandardCharsets.UTF_8));
        ObjectId id = store.write(blob);
        // replace the stored bytes with a deflate of different content
        Path p = store.pathFor(id);
        try (var out = new GitObject.DeflaterStream(java.nio.file.Files.newOutputStream(p))) {
            out.write(GitObject.blob("tampered".getBytes(StandardCharsets.UTF_8)).canonical());
        }
        assertThrows(IOException.class, () -> store.read(id));
    }

    @Test
    @DisplayName("stored objects are deflated, not stored raw")
    void looseObjectsAreCompressed(@TempDir Path dir) throws IOException {
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        byte[] big = new byte[64 * 1024];
        java.util.Arrays.fill(big, (byte) 'a');
        GitObject blob = GitObject.blob(big);
        ObjectId id = store.write(blob);
        long onDisk = java.nio.file.Files.size(store.pathFor(id));
        assertTrue(onDisk < big.length, "repetitive content must compress, was " + onDisk);
    }

    @Test
    @DisplayName("listAll returns ids in ascending order")
    void listAllIsSorted(@TempDir Path dir) throws IOException {
        GitObject.LooseStore store = new GitObject.LooseStore(dir);
        store.write(GitObject.blob("one".getBytes(StandardCharsets.UTF_8)));
        store.write(GitObject.blob("two".getBytes(StandardCharsets.UTF_8)));
        store.write(GitObject.blob("three".getBytes(StandardCharsets.UTF_8)));
        List<ObjectId> all = store.listAll();
        assertEquals(3, all.size());
        for (int i = 1; i < all.size(); i++) {
            assertTrue(all.get(i - 1).compareTo(all.get(i)) < 0, "ids must be sorted");
        }
        assertFalse(all.isEmpty());
    }
}

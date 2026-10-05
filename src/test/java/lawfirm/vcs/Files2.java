package lawfirm.vcs;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.file.Files;

/** Small filesystem helpers shared by tests. */
final class Files2 {

    private Files2() {
    }

    static java.nio.file.Path tempDir(String prefix) {
        try {
            return Files.createTempDirectory(prefix);
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }
}

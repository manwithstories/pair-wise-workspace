#!/bin/zsh
# Compiles main and test sources, then runs the JUnit 5 suite.
# Usage: ./build.sh [--skip-tests]
set -e
cd "$(dirname "$0")"

JUNIT="$PWD/lib/junit-platform-console-standalone-1.10.5.jar"
JGIT="$PWD/lib/org.eclipse.jgit-6.8.0.202311291450-r.jar"
SLF4J="$PWD/lib/slf4j-api-2.0.16.jar"

rm -rf build/classes build/test-classes
mkdir -p build/classes build/test-classes

echo "==> compiling main sources"
javac --release 17 -Xlint:all -d build/classes $(find src/main/java -name '*.java')

echo "==> compiling tests"
javac --release 17 -cp "build/classes:$JUNIT:$JGIT:$SLF4J" -d build/test-classes \
  $(find src/test/java -name '*.java')

if [[ "$1" == "--skip-tests" ]]; then
  echo "==> skipping tests as requested"
  exit 0
fi

echo "==> running JUnit 5 suite"
java -jar "$JUNIT" \
  --class-path "build/classes:build/test-classes:$JUNIT:$JGIT:$SLF4J" \
  --select-package lawfirm.vcs \
  --details=tree \
  --disable-ansi-colors \
  --fail-if-no-tests

#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VENDOR="$ROOT/vendor"
PREFIX="$VENDOR/install"
VERSION=0.17.4

need_pkg() {
    module=$1
    required=$2
    if pkg-config --atleast-version="$required" "$module" 2>/dev/null; then
        return
    fi
    echo "missing or too old: $module >= $required" >&2
    if command -v brew >/dev/null 2>&1; then
        brew install freetype harfbuzz fribidi fontconfig
    elif command -v apt-get >/dev/null 2>&1; then
        sudo apt-get update
        sudo apt-get install -y build-essential pkg-config libfreetype-dev \
            libharfbuzz-dev libfribidi-dev libfontconfig-dev
    else
        echo "install the pkg-config package '$module' and re-run" >&2
        exit 1
    fi
    pkg-config --atleast-version="$required" "$module" || {
        echo "dependency is still too old: $module >= $required" >&2
        exit 1
    }
}

for tool in cmake curl tar make pkg-config; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "missing required tool: $tool" >&2
        exit 1
    }
done

need_pkg freetype2 2.13.0
need_pkg harfbuzz 8.0.0
need_pkg fribidi 0.19.1
need_pkg fontconfig 2.10.92

mkdir -p "$VENDOR"
SRC="$VENDOR/libass-$VERSION"
if [ ! -f "$SRC/configure" ]; then
    TARBALL="$VENDOR/libass-$VERSION.tar.gz"
    if [ ! -f "$TARBALL" ]; then
        curl -fL --retry 3 \
            "https://github.com/libass/libass/releases/download/$VERSION/libass-$VERSION.tar.gz" \
            -o "$TARBALL"
    fi
    tar -xzf "$TARBALL" -C "$VENDOR"
fi

cd "$SRC"
if [ ! -f Makefile ]; then
    ./configure --prefix="$PREFIX" --disable-shared --enable-static \
        --disable-libunibreak
fi
make -j"${JOBS:-4}"
make install

echo "libass $VERSION installed under $PREFIX"
echo "now run: cmake -S '$ROOT' -B '$ROOT/build' -DCMAKE_BUILD_TYPE=Release"

# Vendored dependencies

Run `./scripts/setup_deps.sh` once.  The script downloads the libass 0.17.x
source release into this directory, builds it statically, and installs headers
and the archive into `vendor/install`.

FreeType, HarfBuzz, FriBidi and Fontconfig are resolved through `pkg-config`.
The build requires FreeType 2.13 or newer and HarfBuzz 8.0 or newer.  Generated
source/build directories are intentionally ignored by Git.

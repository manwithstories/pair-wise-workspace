// Placeholder translation unit so the static library exists before the
// real modules land. Replaced as src/*.cpp are added per module batch.
namespace mediacore {
inline constexpr int kAbiRevision = 1;
int abiRevision() { return kAbiRevision; }
}  // namespace mediacore

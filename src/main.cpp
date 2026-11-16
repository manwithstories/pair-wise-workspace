#include <iostream>
#include "Errors.h"
#include "cli/Args.h"
#include "cli/Commands.h"

using namespace lg;

static int dispatch(const Args& args) {
  const std::string& c = args.command();
  if (c == "probe") return cmdProbe(args);
  if (c == "apply") return cmdApply(args);
  if (c == "batch") return cmdBatch(args);
  if (c == "verify") return cmdVerify(args);
  if (c == "demo") return cmdDemo(args);
  throw ArgsError("unknown subcommand: " + c);
}

int main(int argc, char** argv) {
  try {
    Args args(argc, argv);
    return dispatch(args);
  } catch (const ArgsError& e) {
    std::cerr << "error(args): " << e.what() << "\n";
    return kExitArgs;
  } catch (const LutError& e) {
    std::cerr << "error(lut): " << e.what() << "\n";
    return kExitLutParse;
  } catch (const IoError& e) {
    std::cerr << "error(io): " << e.what() << "\n";
    return kExitIo;
  } catch (const ToleranceError& e) {
    std::cerr << "error(tolerance): " << e.what() << "\n";
    return kExitTolerance;
  } catch (const InternalError& e) {
    std::cerr << "error(internal): " << e.what() << "\n";
    return kExitInternal;
  } catch (const std::exception& e) {
    std::cerr << "error(internal): " << e.what() << "\n";
    return kExitInternal;
  }
}

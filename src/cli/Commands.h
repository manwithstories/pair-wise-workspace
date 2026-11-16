#pragma once
#include "cli/Args.h"

namespace lg {

int cmdProbe(const Args& args);
int cmdApply(const Args& args);
int cmdBatch(const Args& args);
int cmdVerify(const Args& args);
int cmdDemo(const Args& args);

} // namespace lg

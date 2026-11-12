raise RuntimeError("fixture must never execute")
import os
from services.safe import unsafe_eval


def load_config():
    token = os.environ.get("TOKEN")
    unsafe_eval(token)

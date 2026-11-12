"""Generate the deterministic 14-file acceptance fixture tree."""

from __future__ import annotations

import shutil
from pathlib import Path


FILES: dict[str, str] = {
    "app.py": """from handlers import route_a, route_b
from services import fetch as load_env
from services.input import fetch_user
from services.middle import relay as forward


def main(request):
    route_a(request)
    route_b(request)
    load_env()
    returned = fetch_user(request)
    forward(returned)
""",
    "config.py": """raise RuntimeError("fixture must never execute")
import os
from services.safe import unsafe_eval


def load_config():
    token = os.environ.get("TOKEN")
    unsafe_eval(token)
""",
    "handlers/__init__.py": """from .routes import route_a, route_b
""",
    "handlers/routes.py": """from flask import request
import os
from services.middle import relay, relay_query


def route_a(req):
    value = request.args.get("q")
    relay(value)
    relay_query(None, value)
    os.system(req)
    return req


def route_b(req):
    return relay_query(None, req)
""",
    "handlers/deep.py": """from flask import request
from services.middle import relay


def deep_flow():
    payload = request.json
    relay(payload)
""",
    "services/__init__.py": """from .input import fetch_env as fetch
from .middle import relay as relay_all
""",
    "services/input.py": """import os
from services.middle import relay


def fetch_user(query):
    return query


def fetch_env():
    value = os.getenv("CMD")
    relay(value)


def fetch_form(req):
    return req.form.get("x")
""",
    "services/middle.py": """from .db import execute_query, run_command


def relay(value):
    return run_command(value)


def relay_query(cursor, value):
    return execute_query(cursor, value)
""",
    "services/db.py": """import os


def run_command(command):
    return os.system(command)


def execute_query(cursor, sql):
    return cursor.execute(sql)
""",
    "services/safe.py": """def unsafe_eval(payload):
    return eval(payload)
""",
    "services/cycle_a.py": """from . import cycle_b


def cycle_a_seed():
    return cycle_b.ping()
""",
    "services/cycle_b.py": """from . import cycle_a


def ping():
    return 1
""",
    "utils/__init__.py": """from .text import render as render_text
""",
    "utils/text.py": """def render(text):
    return eval(text)
""",
}


POLICY_TEXT = """sources:
  functions:
    - input
    - os.getenv
    - os.environ.get
    - request.args.get
    - request.form.get
    - request.values.get
    - request.get_json
  attributes:
    - request.json
    - request.data
  parameters:
    - request
    - query
  environment: true
sinks:
  - name: os.system
  - name: cursor.execute
  - name: eval
  - name: exec
sanitizers:
  - int
  - float
  - shlex.quote
  - html.escape
budgets:
  max_files: 500
  max_summary_rounds: 12
  max_iterations: 2000
  max_paths: 1000
  max_chain: 12
  max_summary_items: 20000
ignore_dirs:
  - .git
  - .cache
  - __pycache__
  - .pytest_cache
  - venv
  - node_modules
"""


def generate(base: str | Path) -> Path:
    base_path = Path(base)
    fixture_root = base_path / "fixtures" / "vuln"
    if fixture_root.exists():
        shutil.rmtree(fixture_root)
    fixture_root.mkdir(parents=True)
    for relpath, text in FILES.items():
        destination = fixture_root / relpath
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(text, encoding="utf-8", newline="\n")
    policy_path = base_path / "policy.yaml"
    policy_path.write_text(POLICY_TEXT, encoding="utf-8", newline="\n")
    return fixture_root


if __name__ == "__main__":
    root = Path(__file__).resolve().parents[1]
    generated = generate(root)
    print(generated)

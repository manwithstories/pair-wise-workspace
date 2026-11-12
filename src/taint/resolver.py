"""Python module discovery, import resolution and module graph construction."""

from __future__ import annotations

import concurrent.futures
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .cache import AstCache, AstNode, CacheEntry, CacheError
from .policy import Policy


@dataclass(frozen=True)
class Parameter:
    name: str
    line: int


@dataclass
class FunctionDef:
    module: str
    module_name: str
    name: str
    qualname: str
    node: AstNode
    body: AstNode | None
    params: tuple[Parameter, ...]
    start_line: int
    class_name: str | None = None

    @property
    def id(self) -> str:
        return f"{self.module}::{self.qualname}"

    @property
    def display_chain_name(self) -> str:
        return self.qualname


@dataclass(frozen=True)
class ImportBinding:
    local: str
    target: str | None
    module_name: str
    line: int
    kind: str
    resolved: bool = True
    symbol: str | None = None


@dataclass
class ModuleInfo:
    relpath: str
    module_name: str
    is_package: bool
    entry: CacheEntry
    root: AstNode
    functions: list[FunctionDef] = field(default_factory=list)
    classes: set[str] = field(default_factory=set)
    imports: list[ImportBinding] = field(default_factory=list)
    exports: dict[str, str] = field(default_factory=dict)
    wildcard_exports: list[str] = field(default_factory=list)
    parse_error: bool = False
    encoding_error: bool = False

    @property
    def source(self) -> bytes:
        return self.entry.source

    @property
    def function_map(self) -> dict[str, FunctionDef]:
        return {function.qualname: function for function in self.functions}


@dataclass
class ModuleGraph:
    root: Path
    modules: dict[str, ModuleInfo]
    by_name: dict[str, str]
    edges: dict[str, set[str]]
    diagnostics: list[dict[str, Any]] = field(default_factory=list)
    files_discovered: int = 0
    cycles: list[tuple[str, ...]] = field(default_factory=list)
    truncated: bool = False

    @property
    def functions(self) -> dict[str, FunctionDef]:
        result: dict[str, FunctionDef] = {}
        for module in self.modules.values():
            for function in module.functions:
                result[function.id] = function
        return result

    def module_by_name(self, name: str) -> ModuleInfo | None:
        relpath = self.by_name.get(name)
        return self.modules.get(relpath) if relpath else None

    def resolve_dotted_symbol(
        self, dotted: str, _visited: set[str] | None = None
    ) -> str | None:
        """Resolve a dotted module/symbol reference to a canonical dotted target.

        Canonical targets are either a module's dotted name or
        ``module.qualname`` for a function/class.  Re-export chains are followed
        with cycle protection.
        """

        if not dotted:
            return None
        visited = _visited or set()
        dotted_marker = f"d:{dotted}"
        if dotted_marker in visited:
            return None
        visited.add(dotted_marker)

        # A module may itself be the target.
        module = self.module_by_name(dotted)
        if module is not None:
            return dotted

        if "." not in dotted:
            return None

        base, symbol = dotted.rsplit(".", 1)
        module = self.module_by_name(base)
        if module is None:
            # Try progressively shorter module prefixes, e.g. pkg.mod.fn.
            parts = dotted.split(".")
            for index in range(len(parts) - 1, 0, -1):
                prefix = ".".join(parts[:index])
                candidate = self.module_by_name(prefix)
                if candidate is None:
                    continue
                rest = parts[index:]
                target = self.resolve_symbol(prefix, rest[0], visited)
                if target is None:
                    return None
                if len(rest) == 1:
                    return target
                return f"{target}.{'.'.join(rest[1:])}"
            return None
        return self.resolve_symbol(base, symbol, visited)

    def resolve_symbol(
        self, module_name: str, symbol: str, _visited: set[str] | None = None
    ) -> str | None:
        visited = _visited or set()
        marker = f"s:{module_name}.{symbol}"
        if marker in visited:
            return None
        visited.add(marker)
        module = self.module_by_name(module_name)
        if module is None:
            return None

        if symbol in module.exports:
            target = module.exports[symbol]
            return self.resolve_dotted_symbol(target, visited)
        if symbol in module.classes:
            return f"{module_name}.{symbol}"
        if any(function.qualname == symbol for function in module.functions):
            return f"{module_name}.{symbol}"

        submodule = f"{module_name}.{symbol}"
        if self.module_by_name(submodule) is not None:
            return submodule

        for exported in module.wildcard_exports:
            target = self.resolve_symbol(module_name, exported, visited)
            if target is not None and target.rsplit(".", 1)[-1] == symbol:
                return target
        return None

    def function_by_dotted(self, dotted: str) -> FunctionDef | None:
        resolved = self.resolve_dotted_symbol(dotted)
        if resolved is None:
            return None
        return self.function_by_canonical(resolved)

    def function_by_canonical(self, canonical: str) -> FunctionDef | None:
        for function in self.functions.values():
            if f"{function.module_name}.{function.qualname}" == canonical:
                return function
        return None


def _decode(node: AstNode) -> str:
    return node.text.decode("utf-8", errors="replace")


def _first_error_line(root: AstNode) -> int:
    stack = [root]
    while stack:
        node = stack.pop(0)
        if node.type == "ERROR" or (hasattr(node, "is_error") and node.is_error):
            return node.start_point[0] + 1
        stack.extend(node.named_children)
    return 1


def _module_dotted(relpath: str) -> tuple[str, bool]:
    path = Path(relpath)
    parts = list(path.parts[:-1]) + [path.stem]
    is_package = path.name == "__init__.py"
    if is_package:
        parts = parts[:-1]
    return ".".join(parts), is_package


def _package_name(module_name: str, is_package: bool) -> str:
    if is_package:
        return module_name
    return module_name.rsplit(".", 1)[0] if "." in module_name else ""


def _function_parameters(function_node: AstNode) -> tuple[Parameter, ...]:
    params_node = function_node.child_by_field_name("parameters")
    if params_node is None:
        return ()
    result: list[Parameter] = []
    for child in params_node.named_children:
        name_node: AstNode | None = None
        if child.type in {
            "identifier",
            "typed_parameter",
            "default_parameter",
            "typed_default_parameter",
        }:
            if child.type == "identifier":
                name_node = child
            else:
                name_node = child.child_by_field_name("name")
        elif child.type in {"list_splat_pattern", "dictionary_splat_pattern"}:
            identifiers = [
                node for node in child.named_children if node.type == "identifier"
            ]
            if identifiers:
                name_node = identifiers[0]
        if name_node is None:
            identifiers = [
                node for node in child.named_children if node.type == "identifier"
            ]
            name_node = identifiers[0] if identifiers else None
        if name_node is not None:
            result.append(Parameter(_decode(name_node), name_node.start_point[0] + 1))
    return tuple(result)


def _unwrap_definition(node: AstNode) -> AstNode:
    if node.type == "decorated_definition":
        definition = node.child_by_field_name("definition")
        if definition is not None:
            return definition
        for child in node.named_children:
            if child.type in {"function_definition", "class_definition"}:
                return child
    return node


def _collect_definitions(module: ModuleInfo) -> None:
    def walk_container(container: AstNode, class_prefix: str | None) -> None:
        for child in container.named_children:
            target = _unwrap_definition(child)
            if target.type == "function_definition":
                name_node = target.child_by_field_name("name")
                name = _decode(name_node) if name_node is not None else "<anonymous>"
                qualname = f"{class_prefix}.{name}" if class_prefix else name
                module.functions.append(
                    FunctionDef(
                        module=module.relpath,
                        module_name=module.module_name,
                        name=name,
                        qualname=qualname,
                        node=target,
                        body=target.child_by_field_name("body"),
                        params=_function_parameters(target),
                        start_line=target.start_point[0] + 1,
                        class_name=class_prefix,
                    )
                )
            elif target.type == "class_definition":
                name_node = target.child_by_field_name("name")
                name = _decode(name_node) if name_node is not None else "<anonymous>"
                module.classes.add(name)
                body = target.child_by_field_name("body")
                if body is not None:
                    walk_container(body, name)

    walk_container(module.root, None)


def _relative_module(
    module_name: str,
    is_package: bool,
    import_text: str,
) -> tuple[str | None, str | None, int]:
    """Return (base module, remaining absolute suffix, dot count)."""

    text = import_text.strip()
    dots = len(text) - len(text.lstrip("."))
    suffix = text[dots:]
    package = _package_name(module_name, is_package)
    if dots <= 0:
        return suffix, None, 0
    if not package:
        return None, "relative import outside package", dots
    base = package
    for _ in range(dots - 1):
        if not base or "." not in base:
            return None, "relative import beyond top-level package", dots
        base = base.rsplit(".", 1)[0]
    if suffix:
        base = f"{base}.{suffix}" if base else suffix
    return base, None, dots


def _collect_imports(module: ModuleInfo) -> list[dict[str, Any]]:
    diagnostics: list[dict[str, Any]] = []
    for node in module.root.named_children:
        if node.type == "import_statement":
            for part in node.named_children:
                if part.type == "dotted_name":
                    target = _decode(part)
                    local = target.split(".", 1)[0]
                    module.imports.append(
                        ImportBinding(
                            local,
                            target,
                            target,
                            node.start_point[0] + 1,
                            "module",
                            symbol=local,
                        )
                    )
                elif part.type == "aliased_import":
                    name_node = part.child_by_field_name("name")
                    alias_node = part.child_by_field_name("alias")
                    if name_node is None or alias_node is None:
                        named = part.named_children
                        name_node = named[0] if named else None
                        alias_node = named[-1] if len(named) > 1 else None
                    if name_node is None or alias_node is None:
                        continue
                    target = _decode(name_node)
                    module.imports.append(
                        ImportBinding(
                            _decode(alias_node),
                            target,
                            target,
                            node.start_point[0] + 1,
                            "module",
                            symbol=_decode(name_node),
                        )
                    )
        elif node.type == "import_from_statement":
            children = node.named_children
            if not children:
                continue
            module_spec_node = children[0]
            if module_spec_node.type not in {"dotted_name", "relative_import"}:
                continue
            spec_text = _decode(module_spec_node)
            if module_spec_node.type == "relative_import":
                base, error, _dots = _relative_module(
                    module.module_name, module.is_package, spec_text
                )
                if error is not None:
                    diagnostics.append(
                        {
                            "severity": "warning",
                            "code": "relative_import",
                            "message": error,
                            "file": module.relpath,
                            "line": node.start_point[0] + 1,
                        }
                    )
                    continue
                if base is None:
                    continue
            else:
                base = spec_text
            import_parts = children[1:]
            for part in import_parts:
                if part.type == "wildcard_import":
                    module.wildcard_exports.append("*")
                    module.imports.append(
                        ImportBinding(
                            "*",
                            base,
                            base,
                            node.start_point[0] + 1,
                            "wildcard",
                            True,
                            "*",
                        )
                    )
                    continue
                if part.type == "dotted_name":
                    symbol = _decode(part)
                    resolved = _resolve_from_symbol(module, base, symbol)
                    module.imports.append(
                        ImportBinding(
                            symbol,
                            f"{base}.{symbol}" if base else symbol,
                            base,
                            node.start_point[0] + 1,
                            "from",
                            resolved,
                            symbol,
                        )
                    )
                elif part.type == "aliased_import":
                    name_node = part.child_by_field_name("name")
                    alias_node = part.child_by_field_name("alias")
                    if name_node is None or alias_node is None:
                        named = part.named_children
                        name_node = named[0] if named else None
                        alias_node = named[-1] if len(named) > 1 else None
                    if name_node is None or alias_node is None:
                        continue
                    symbol = _decode(name_node)
                    local = _decode(alias_node)
                    resolved = _resolve_from_symbol(module, base, symbol)
                    module.imports.append(
                        ImportBinding(
                            local,
                            f"{base}.{symbol}" if base else symbol,
                            base,
                            node.start_point[0] + 1,
                            "from",
                            resolved,
                            symbol,
                        )
                    )
    return diagnostics


def _resolve_from_symbol(module: ModuleInfo, base: str, symbol: str) -> bool:
    # Resolution against the complete graph happens after all modules are
    # parsed; here we only know package-local re-exports and submodules.
    if base in {"", module.module_name}:
        return symbol in module.classes or any(
            f.qualname == symbol for f in module.functions
        )
    return True


def _exports_from_imports(module: ModuleInfo, graph_pending: bool = True) -> None:
    for binding in module.imports:
        if binding.kind in {"from", "wildcard"}:
            if binding.target:
                if binding.kind == "from":
                    module.exports.setdefault(binding.local, binding.target)
        else:
            module.exports.setdefault(binding.local, binding.target or binding.local)


def _module_summary(module: ModuleInfo) -> dict[str, Any]:
    return {
        "module_name": module.module_name,
        "is_package": module.is_package,
        "classes": sorted(module.classes),
        "functions": [
            {
                "name": function.name,
                "qualname": function.qualname,
                "line": function.start_line,
                "params": [[param.name, param.line] for param in function.params],
            }
            for function in module.functions
        ],
        "imports": [
            {
                "local": binding.local,
                "target": binding.target,
                "module": binding.module_name,
                "line": binding.line,
                "kind": binding.kind,
            }
            for binding in module.imports
        ],
        "exports": dict(sorted(module.exports.items())),
    }


def _build_module(
    relpath: str, entry: CacheEntry, diagnostics: list[dict[str, Any]]
) -> ModuleInfo:
    module_name, is_package = _module_dotted(relpath)
    source_text_ok = True
    try:
        entry.source.decode("utf-8")
    except UnicodeDecodeError:
        source_text_ok = False
        diagnostics.append(
            {
                "severity": "warning",
                "code": "encoding_error",
                "message": "source is not valid UTF-8; parsed bytes with replacement only for analysis text",
                "file": relpath,
                "line": 1,
            }
        )
    module = ModuleInfo(
        relpath=relpath,
        module_name=module_name,
        is_package=is_package,
        entry=entry,
        root=entry.tree.root_node,
        parse_error=entry.tree.has_error,
        encoding_error=not source_text_ok,
    )
    if entry.tree.has_error:
        diagnostics.append(
            {
                "severity": "warning",
                "code": "syntax_error",
                "message": "tree-sitter recovered a syntax-error node",
                "file": relpath,
                "line": _first_error_line(module.root),
            }
        )
    _collect_definitions(module)
    diagnostics.extend(_collect_imports(module))
    _exports_from_imports(module)
    return module


def _discover_files(
    root: Path, policy: Policy
) -> tuple[list[Path], int, list[dict[str, Any]]]:
    ignored = set(policy.ignore_dirs)
    candidates: list[Path] = []
    for current, dirs, files in os.walk(root):
        dirs[:] = sorted(directory for directory in dirs if directory not in ignored)
        for filename in sorted(files):
            if filename.endswith(".py"):
                candidates.append(Path(current) / filename)
    candidates.sort(key=lambda item: item.as_posix())
    diagnostics: list[dict[str, Any]] = []
    files_discovered = len(candidates)
    if len(candidates) > policy.budgets.max_files:
        candidates = candidates[: policy.budgets.max_files]
        diagnostics.append(
            {
                "severity": "warning",
                "code": "file_budget",
                "message": f"file budget exceeded: scanned {policy.budgets.max_files} of {files_discovered}",
            }
        )
    return candidates, files_discovered, diagnostics


def _tarjan_cycles(edges: dict[str, set[str]]) -> list[tuple[str, ...]]:
    index = 0
    indices: dict[str, int] = {}
    low: dict[str, int] = {}
    stack: list[str] = []
    on_stack: set[str] = set()
    cycles: list[tuple[str, ...]] = []

    def visit(node: str) -> None:
        nonlocal index
        indices[node] = index
        low[node] = index
        index += 1
        stack.append(node)
        on_stack.add(node)
        for target in sorted(edges.get(node, ())):
            if target not in edges:
                continue
            if target not in indices:
                visit(target)
                low[node] = min(low[node], low[target])
            elif target in on_stack:
                low[node] = min(low[node], indices[target])
        if low[node] == indices[node]:
            component: list[str] = []
            while True:
                item = stack.pop()
                on_stack.remove(item)
                component.append(item)
                if item == node:
                    break
            if len(component) > 1 or (
                len(component) == 1 and node in edges.get(node, ())
            ):
                cycles.append(tuple(sorted(component)))

    for node in sorted(edges):
        if node not in indices:
            visit(node)
    return sorted(cycles, key=lambda value: (len(value), value))


def _finalize_imports(graph: ModuleGraph) -> None:
    for module in graph.modules.values():
        for binding in module.imports:
            if binding.kind != "from" or binding.target is None:
                continue
            base = binding.module_name
            symbol = binding.symbol or binding.local
            if base and graph.module_by_name(base) is not None:
                target = graph.resolve_symbol(base, symbol)
                if target is None and f"{base}.{symbol}" not in graph.by_name:
                    graph.diagnostics.append(
                        {
                            "severity": "warning",
                            "code": "unresolved_symbol",
                            "message": f"cannot resolve {binding.local!r} from {base}",
                            "file": module.relpath,
                            "line": binding.line,
                        }
                    )
        for exported, target in list(module.exports.items()):
            resolved = graph.resolve_dotted_symbol(target)
            if resolved is not None:
                module.exports[exported] = resolved
        # Materialize wildcard imports from package __init__ exports.
        if "*" in module.wildcard_exports:
            module.wildcard_exports.remove("*")
            for binding in module.imports:
                if binding.kind != "wildcard" or binding.target is None:
                    continue
                base = binding.module_name
                base_module = graph.module_by_name(base)
                if base_module is None:
                    continue
                for name in sorted(base_module.exports):
                    module.wildcard_exports.append(name)
                    module.exports.setdefault(name, base_module.exports[name])


def build_module_graph(
    root: str | Path,
    policy: Policy,
    cache: AstCache,
    jobs: int = 0,
) -> ModuleGraph:
    root_path = Path(root).resolve()
    if not root_path.is_dir():
        raise CacheError(f"scan root is not a directory: {root}")
    files, files_discovered, diagnostics = _discover_files(root_path, policy)

    loaded: dict[str, CacheEntry] = {}
    parse_diagnostics: list[dict[str, Any]] = []
    workers = min(max(1, jobs), 16) if jobs > 0 else min(16, max(1, len(files)))
    if files:
        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
            future_map = {executor.submit(cache.load, path): path for path in files}
            for future in concurrent.futures.as_completed(future_map):
                path = future_map[future]
                relpath = cache.relative_path(path)
                try:
                    loaded[relpath] = future.result()
                except (OSError, CacheError) as exc:
                    parse_diagnostics.append(
                        {
                            "severity": "error",
                            "code": "parse_failure",
                            "message": f"cannot parse file: {exc}",
                            "file": relpath,
                            "line": 1,
                        }
                    )

    # Stable merge order is independent of thread completion order.
    modules: dict[str, ModuleInfo] = {}
    for relpath in sorted(loaded):
        module_diagnostics: list[dict[str, Any]] = []
        modules[relpath] = _build_module(relpath, loaded[relpath], module_diagnostics)
        diagnostics.extend(module_diagnostics)

    by_name = {module.module_name: relpath for relpath, module in modules.items()}
    edges: dict[str, set[str]] = {relpath: set() for relpath in modules}
    for relpath, module in modules.items():
        for binding in module.imports:
            targets = [binding.target or "", binding.module_name]
            for target in targets:
                if not target:
                    continue
                candidate = target
                while candidate:
                    if candidate in by_name:
                        edges[relpath].add(by_name[candidate])
                        break
                    if "." not in candidate:
                        break
                    candidate = candidate.rsplit(".", 1)[0]

    graph = ModuleGraph(
        root=root_path,
        modules=modules,
        by_name=by_name,
        edges=edges,
        diagnostics=diagnostics,
        files_discovered=files_discovered,
        truncated=len(files) < files_discovered,
    )
    cycles = _tarjan_cycles(edges)
    graph.cycles = cycles
    for cycle in cycles:
        names = sorted({modules[relpath].module_name for relpath in cycle})
        graph.diagnostics.append(
            {
                "severity": "warning",
                "code": "cyclic_import",
                "message": "cyclic import: " + " -> ".join(names),
                "file": modules[cycle[0]].relpath,
                "line": 1,
            }
        )

    _finalize_imports(graph)

    for module in modules.values():
        cache.store_summary(module.entry, _module_summary(module))
    return graph

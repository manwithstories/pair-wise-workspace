"""混音 DAG：总线拓扑、静音/独奏语义与成环检测。

图节点由“轨道 -> 总线 -> 上级总线 -> 主总线”构成。轨道不属于图的
拓扑节点，它们直接把信号送进声明的总线；总线之间按 Kahn 算法求
拓扑序，检测到环时抛出带字段路径的 GraphError（退出码 4）。

独奏语义：任一轨道进入独奏集合时，其余轨道在混音阶段归零，但它们
的指标仍在引擎里按各自原始信号单独统计。
"""

from __future__ import annotations

import bisect
from dataclasses import dataclass

from .errors import GraphError
from .project import Project


@dataclass
class MixGraph:
    project: Project
    order: list[str]                 # 总线名的拓扑执行顺序
    bus_outputs: dict[str, str | None]
    bus_incoming: dict[str, list[str]]
    track_bus: dict[str, str]
    solo_tracks: set[str]
    muted_tracks: set[str]

    @property
    def master(self) -> str:
        return self.project.master_bus

    def is_audible(self, track_name: str) -> bool:
        """该轨道是否参与混音（独奏/静音判定）。"""
        if track_name in self.muted_tracks:
            return False
        if self.solo_tracks:
            return track_name in self.solo_tracks
        return True


def build_graph(project: Project) -> MixGraph:
    bus_outputs = {bus.name: bus.output for bus in project.buses}
    bus_incoming: dict[str, list[str]] = {bus.name: [] for bus in project.buses}

    track_bus: dict[str, str] = {}
    muted: set[str] = set()
    solo: set[str] = set()
    for track in project.tracks:
        track_bus[track.name] = track.bus
        if track.muted:
            muted.add(track.name)
        if track.solo:
            solo.add(track.name)

    indegree = {name: 0 for name in bus_outputs}
    for name, output in bus_outputs.items():
        if output is None:
            if name != project.master_bus:
                raise GraphError(
                    f"buses[{_bus_index(project, name)}].output: "
                    f"非主总线 {name!r} 不能悬空"
                )
            continue
        if output == name:
            raise GraphError(
                f"buses[{_bus_index(project, name)}].output: 总线 {name!r} 不能接到自身"
            )
        # 边 name -> output：output 的入度来自 name。
        indegree[output] += 1
        bus_incoming[output].append(name)

    order = _kahn_toposort(bus_outputs, indegree, project)
    if order[-1] != project.master_bus:
        raise GraphError(
            f"$.master_bus: 主总线 {project.master_bus!r} 不是拓扑终点，"
            "存在无法汇入主总线的分支"
        )

    return MixGraph(
        project=project,
        order=order,
        bus_outputs=bus_outputs,
        bus_incoming=bus_incoming,
        track_bus=track_bus,
        solo_tracks=solo,
        muted_tracks=muted,
    )


def _bus_index(project: Project, name: str) -> int:
    for index, bus in enumerate(project.buses):
        if bus.name == name:
            return index
    return -1


def _kahn_toposort(
    bus_outputs: dict[str, str | None],
    indegree: dict[str, int],
    project: Project,
) -> list[str]:
    # 边 name -> output（信号从 name 流向 output），先执行无入边的叶子总线。
    remaining = dict(indegree)
    queue = sorted(name for name, degree in remaining.items() if degree == 0)
    order: list[str] = []
    while queue:
        current = queue.pop(0)
        order.append(current)
        target = bus_outputs[current]
        if target is not None:
            remaining[target] -= 1
            if remaining[target] == 0:
                bisect.insort(queue, target)
    if len(order) != len(bus_outputs):
        cycle_nodes = sorted(name for name, degree in remaining.items() if degree > 0)
        path = ".".join(cycle_nodes[:4])
        raise GraphError(f"$.buses: 总线成环，环上节点: {path}")
    return order

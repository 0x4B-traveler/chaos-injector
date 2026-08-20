# Architecture — 四层混沌工程架构的轻量级落地（Python + Go 双实现）

本文档描述 `chaos-injector` 如何将工业级混沌工程平台的**四层架构**
（环境感知 → 随机决策 → 故障执行 → 恢复监控）精简为可独立运行的最小实现，
并保持相同的安全保证。项目提供 **Python 与 Go 两套行为等价**的实现：
Python 版以 3 个模块零依赖落地；Go 版以标准库单二进制落地，
为后续基于 netlink 直操作内核预留路径。

## 1. 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│   CLI：chaos.py (Python)  /  main.go (Go)                    │
│       network / cpu / mem / process / port / random          │
└───────────────┬───────────────────────────────┬──────────────┘
                │                               │
                ▼                               ▼
┌───────────────────────┐         ┌───────────────────────────┐
│  随机决策层 (随机决策)  │         │    环境感知层 (环境感知)    │
│  random 子命令：        │         │  EnvProbe / LoadGate:     │
│  · 安全参数范围随机      │────────►│  · loadavg 负载熔断        │
│  · 故障类型随机         │         │  · 平台/工具/权限预检       │
└───────────────────────┘         └─────────────┬─────────────┘
                                                │
                                                ▼
┌─────────────────────────────────────────────────────────────┐
│                    故障执行层 (故障执行)                       │
│  BaseFault / fault.Fault 统一生命周期：check → inject →      │
│  recover                                                 │
│  ├── NetworkFault   tc qdisc netem（延迟/丢包，Linux root）   │
│  ├── CpuFault       stress-ng / 纯 Python 烧核 / goroutine   │
│  │                  烧核（跨平台）                            │
│  ├── MemFault       内存缓冲 + 逐页触摸保持驻留（OOM 保护）    │
│  ├── ProcessFault   /proc 扫描 comm/cmdline 匹配 → SIGKILL， │
│  │                  保护自身与祖先链                          │
│  ├── PortFault      TCP listen 占用端口，恢复即释放          │
│  ├── PodKillFault   kubectl delete pod（恢复=K8s 自愈）       │
│  └── MySQLFault     mysql CLI 注入：连接池占用 / 表锁持有 /  │
│                     会话 KILL；密码走 MYSQL_PWD，零泄露      │
└───────────────────────────────┬─────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────┐
│                    恢复监控层 (恢复与监控)                     │
│  Experiment（scheduler.py / experiment.go）                  │
│  ├── threading.Timer / time.AfterFunc 定时自动回滚            │
│  ├── 上下文管理器 / 幂等 Recover 兜底（Ctrl-C/异常必回滚）      │
│  └── JSON 时间线证据（check/inject/armed/recover 全记录）      │
└─────────────────────────────────────────────────────────────┘
```

## 2. 故障生命周期

```
                ┌──────────┐
                │   start  │
                └────┬─────┘
                     ▼
                ┌──────────┐      FaultError（平台不支持/权限不足）
                │  check   │ ───────────────────────────► 记录 error，终止
                └────┬─────┘
                     ▼
                ┌──────────┐
                │  inject  │ ────► 故障生效，进入观察窗口
                └────┬─────┘
                     ▼
        ┌────────────┼────────────┐
        ▼            ▼            ▼
   Timer 到期     Ctrl-C/异常    手动 recover()
   (auto)        (上下文管理器)   (幂等)
        └────────────┼────────────┘
                     ▼
                ┌──────────┐
                │  recover │ 幂等回滚，绝不抛异常
                └────┬─────┘
                     ▼
                ┌──────────┐
                │ timeline │ 全事件落盘为 JSON 证据
                └──────────┘
```

**回滚三重保障**（与工业平台“统一清理恢复接口”同源的设计决策）：

1. `threading.Timer` / `time.AfterFunc(duration, recover)` — 正常路径，到期自动回滚；
2. `Experiment.__exit__` / 幂等 `Recover()` — 任何中断/异常路径都会执行回滚；
3. `recover()` 幂等 — 重复调用安全，`tc qdisc del` 的“已不存在”rc=2 视为成功。

## 3. 模块职责

| 语言 | 模块 | 对应层级 | 职责 |
|---|---|---|---|
| Python | `chaos_injector/faults.py` | 故障执行层 + 环境感知 | 故障原子统一生命周期（check/inject/recover）、`EnvProbe` 负载熔断、`FAULTS` 注册表（7 原子含 pod-kill / mysql） |
| Python | `chaos_injector/scheduler.py` | 恢复监控层 | 实验编排：定时回滚、上下文管理器、JSON 时间线证据 |
| Python | `chaos_injector/chaos.py` | 随机决策层 + 入口 | CLI 参数解析、`random` 安全范围随机编排、`--confirm` 安全闸门 |
| Python | `chaos_injector/k8s.py` | Kubernetes 编排层 | kubectl 封装（minikube 回退）、Pod 内代理注入（cp + chmod + exec + 时间线回拷） |
| Go | `internal/fault/` | 故障执行层 + 环境感知 | `Fault` 接口与 `Registry` 注册表、7 个故障原子（network/cpu/mem/process/port/podkill/mysql）、`LoadGate` 熔断 |
| Go | `internal/experiment/` | 恢复监控层 | 实验编排：`time.AfterFunc` 自动回滚、幂等 `Recover`、JSON 时间线 |
| Go | `internal/k8s/` | Kubernetes 编排层 | kubectl 客户端（注入式 Runner 便于测试）、`RunPodExperiment` 代理编排（client.go / exec.go） |
| Go | `cmd/chaos-injector/` | 随机决策层 + 入口 | 单二进制 CLI：network / cpu / mem / process / port / mysql / k8s（pod-kill / exec）/ random、`-confirm` 安全闸门 |

## 4. 安全边界

- **拒绝注入**：无 `--confirm`、平台不支持、无 root、主机负载超标 —— 全部拒绝执行；
- **最小爆炸半径**：网络故障仅作用于指定网卡；CPU 故障默认仅 1 核；
- **自我保护**：进程故障通过 `/proc/<pid>/status` PPid 回溯排除自身与祖先链；
  内存故障在 Linux 下按 `MemAvailable` 校验，拒绝超过可用内存的注入（防 OOM）；
- **证据留存**：每个实验的时间线（start/check/inject/armed/recover/done）可落盘 JSON，
  并记录随机种子（`random --seed N`，双端共享 xorshift64* PRNG，同种子同实验）
  与注入前后系统快照（hostname/loadavg/可用内存），支持事后回放与跨端对比（稳定复现），
  与工业平台“故障注入 → 证据采集 → 恢复”的实验方法论一致。

**Kubernetes 注入（v0.3 新增）**：

- **双重确认闸门**：编排器与 Pod 内代理各需一次 `--confirm` / `-confirm`，缺一拒绝注入；
- **Pod 内自动回滚**：代理自带 `time.AfterFunc` 定时回滚，编排器中途崩溃不影响恢复（代理在 Pod 内独立存活）；
- **pod-kill 不可逆**：删除的 Pod 无法恢复，恢复即 Kubernetes 自愈（Deployment 重建），
  `--wait-ready` 把“等待替换 Pod Ready”纳入实验，验证自愈闭环；
- **最小特权**：cpu / mem / process / port 无特权要求；network 需 privileged（CAP_NET_ADMIN）+ 镜像含 `tc`；
  mysql 原子仅需目标 Pod 内有 mysql 客户端；
- **数据库密码零泄露**：密码只经 `MYSQL_PWD` 环境变量传递，不出现在命令行、`describe()`、时间线与任何日志；
  库表名经 `[A-Za-z0-9_]+` 白名单校验防 SQL 注入；连接数预检不得超过 `max_connections`；
- **session 模式不可逆**：会话 KILL 无法回滚（同 process 原子），recover 为 no-op；connection/lock 模式恢复即杀 holder 进程（连接与表锁随之释放）。

## 5. 演进路线

| 阶段 | 内容 |
|---|---|
| v0.1（已完成） | Python 版：网络/CPU 故障原子 + 自动回滚 + 随机编排 + 时间线 |
| v0.2（已完成） | Go 版：标准库重写，行为与 Python 对齐（时间线事件一致），单二进制分发 |
| v0.25（已完成） | 参考 chaosblade 场景新增 mem / process / port 三类故障原子，双端实现 + minikube 实机验证 |
| v0.3（已完成） | Kubernetes Pod 级注入：k8s 子命令（pod-kill / exec）、编排器 + Pod 内 Go 代理、双重确认闸门、minikube 实机验证 8 项 |
| v0.35（已完成） | 稳定复现：`random --seed` 双端共享 xorshift64* PRNG（同种子同实验）+ 时间线记录注入前后系统快照 |
| v0.4（已完成） | 数据库实例故障原子 mysql（connection/lock/session 三模式），双端实现 + minikube MySQL 8 实机验证 |
| v0.5（计划） | 混沌实验 CRD / 控制器化（对标 chaos-mesh）、Go 基于 netlink 直操作内核（无 shell 依赖）、IO 延迟原子 |
| v0.6（计划） | Prometheus 可观测性对接、随机编排概率分布 |

## 6. 与工业平台的关系

本项目不是玩具复刻，而是工业平台核心思想的**最小可验证载体**：

- 相同的四层架构（本项目是其逐层压缩版，Python/Go 双实现行为对齐）；
- 相同的故障原子抽象（`BaseFault` / `Fault` 统一生命周期对应工业平台的统一清理恢复接口）；
- 相同的实验方法论（注入 → 观察 → 证据 → 回滚）。

差异在于：工业平台面向 GaussDB 长稳测试，封装 20+ 故障原子并集成 Jenkins 无人值守运行；
本项目面向本地开发与学习，优先保证“一次实验、零残留、可复现”。

## 7. 与 chaosblade 的场景映射

新增故障原子参考了 chaosblade（阿里开源混沌工程工具）的经典场景，映射关系：

| chaosblade 命令 | 本实现原子 | 差异说明 |
|---|---|---|
| `blade create network delay/loss` | `network` | 同为 `tc netem`，本实现限定单网卡 + 自动回滚 |
| `blade create cpu fullload` | `cpu` | 本实现支持 `--load 30-90` 部分占用与纯 Python 烧核回退 |
| `blade create mem load` | `mem` | 本实现按可用内存校验，拒绝可能触发 OOM 的参数 |
| `blade create process kill` | `process` | 本实现仅按 pattern 匹配，且排除自身与祖先链 |
| `blade create network port occupy` | `port` | 同为 TCP listen 占端口，恢复即释放 |
| `blade create k8s pod-kill` | `pod-kill` | 恢复即 Kubernetes 自愈；`wait-ready` 等待替换 Pod Ready 验证自愈 |
| `blade create mysql` | `mysql` | 本实现三模式（connection/lock/session），密码走 `MYSQL_PWD` 环境变量（chaosblade 同样支持），库表名白名单校验防注入 |
| `blade create k8s pod-cpu/pod-mem/...` | `k8s exec <atom>` | 同构：编排器将代理注入 Pod 内执行既有原子（chaosblade 为每原子独立 k8s 命令） |

选择这三个场景的原因：与现有 `tc`/纯标准库实现同构（不引入 dm-device / iptables 等
内核模块依赖），可在 minikube 上真实注入并验证回滚。

## 8. 实机验证

minikube（VirtualBox 驱动，2 核 / 5.9GB 内存）真实注入验证全部 6 个原子，证据留存于
`artifacts/minikube/`（net / cpu / mem / port / proc / rand 六个 `*-exp.json`）：
网络延迟经 `ping` 对比（0.28ms → 100.18ms → 0.24ms）、CPU 经 top 观察
（0% → 80% → 0%）、内存经 `free -m`（已用 1835MB → 2089MB → 1825MB）、
端口经 `/dev/tcp` 探测（CLOSED → OPEN → CLOSED）、进程经后台 `sleep 1000` 被
SIGKILL 验证（TARGET_KILLED），每次实验时间线均完整记录自动回滚事件。

### 8.1 Kubernetes 注入验证（v0.3）

在相同 minikube 上完成 Pod 级注入闭环（证据 `artifacts/minikube/k8s/`）：

- **pod-kill 自愈**（Go + Python 双端）：删除 Deployment 下 Pod，`wait-ready` 观察到替换 Pod Ready
  （`chaos-demo-85576855b4-tc89n` / `f949t`），时间线完整记录；
- **Pod 内原子注入**（Go 端 5 原子 + Python 端 cpu）：Linux 版 Go 代理经 `kubectl cp` 进 busybox Pod，
  cpu（80%×1 核 4s）/ mem（64MB 4s）/ port（8080 4s）/ process（杀死 Pod 内 `sleep 9999`）全部
  在 Pod 内注入并自动回滚；
- **特权 Pod 网络**：`nicolaka/netshoot` 特权 Pod 内注入 100ms 延迟，实测 ping RTT
  0.04ms → 100.06ms → 0.03ms（busybox 无 `tc`，首次注入被正确拒绝并记录 error 事件）；
- **失败路径验证**：busybox 镜像缺 `tc` 时 network 注入以 error 事件终止、无残留；
  Pod 内代理缺 `-confirm` 时拒绝注入（双重闸门生效）。

### 8.2 数据库实例注入验证（v0.4，mysql 原子）

在 minikube 上部署 MySQL 8（`mysql:8` Pod，内含 mysql 客户端），经 `k8s exec` 在 Pod 内
完成三种数据库故障的真实注入闭环（证据 `artifacts/minikube/k8s/mysql-*`）：

- **connection**：占用 5 个连接 25s，`Threads_connected` 实测 1 → 6（5 个 `SELECT SLEEP(25)`
  holder 可见于 processlist）→ 自动恢复回 1，时间线含注入前后快照；
- **lock**：持有 `app.orders` 表写锁 30s，期间并发 INSERT 阻塞于
  `Waiting for table metadata lock`，锁释放后写入成功，processlist 无残留；
- **session**：后台 `SELECT SLEEP(60)` 受害会话被按 SQL 模式匹配 KILL，客户端立即报
  ERROR 2013 中断（远早于 60s 自然结束），验证了不可逆类故障的观察与证据留存。

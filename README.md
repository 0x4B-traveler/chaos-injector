# Chaos Injector —— 轻量级混沌实验引擎（Python + Go 双实现）

[![Python 3.10+](https://img.shields.io/badge/python-3.10%2B-blue)](https://www.python.org/)
[![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-blue)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Windows-lightgrey)]()
[![CI](https://github.com/your-name/chaos-injector/actions/workflows/ci.yml/badge.svg)]()

## 设计理念

受作者在华为高斯数据库（GaussDB）高可用容灾测试中设计的**四层混沌工程架构**启发，本项目将其核心思想精简提炼，实现了一个轻量级的故障注入引擎，用于本地开发环境或小型分布式系统的韧性验证。

工业级平台关注的是"长稳测试中无人值守地持续注入随机故障"；本项目的关注点是"**一次实验、最小实现、保证回滚**"——把故障注入的完整闭环（环境感知 → 随机决策 → 故障执行 → 恢复监控）压缩到最小实现。项目提供 **Python 与 Go 两套等价实现**：

- `python/`：零第三方依赖的原型，4 个模块，适合快速实验与教学；
- `go/`：单二进制分发，标准库实现，`time.AfterFunc` 定时回滚，为后续基于 netlink 直操作内核、去掉 shell 依赖预留路径。

## 四层架构映射

| 工业平台四层 | Python 实现 | Go 实现 |
|---|---|---|
| **环境感知** | `EnvProbe`：`os.getloadavg()` 负载熔断（Linux），`--dry-run` 预检平台/工具/权限 | `LoadGate`：读取 `/proc/loadavg` 熔断，`-dry-run` 预检 |
| **随机决策** | `chaos random`：安全范围内随机故障类型与参数（延迟 50-500ms、丢包 1-10%、CPU 30-90%、内存 64-256MB、端口 8000-9999） | `random` 子命令：同样的安全参数范围（`math/rand/v2`） |
| **故障执行** | `BaseFault` 统一生命周期：`check → inject → recover`；`tc netem` / `stress-ng` / 内存缓冲 / 端口监听 / SIGKILL | `fault.Fault` 接口：`Check → Inject → Recover`；`tc netem` / `stress-ng` / 内存缓冲 / 端口监听 / SIGKILL |
| **恢复监控** | `threading.Timer` 自动回滚 + 上下文管理器兜底 + JSON 时间线 | `time.AfterFunc` 自动回滚 + 幂等 `Recover` + JSON 时间线 |

## 已支持的故障原子

| 故障 | 参考 chaosblade 场景 | 实现方式 | 平台 | Python | Go |
|---|---|---|---|---|---|
| 网络延迟 / 丢包 | `blade create network delay/loss` | `tc qdisc ... netem` | Linux（需 root + iproute2） | ✅ | ✅ |
| CPU 资源耗尽 | `blade create cpu fullload` | `stress-ng --cpu-load`；回退纯 Python 烧核 / goroutine 烧核 | Linux / Windows | ✅ | ✅ |
| 内存占用 | `blade create mem load` | 分配缓冲 + 后台逐页触摸保持驻留；Linux 下按可用内存做 OOM 保护 | Linux / Windows | ✅ | ✅ |
| 进程终止 | `blade create process kill` | `/proc` 扫描 comm/cmdline 匹配 → SIGKILL；保护自身与祖先链 | Linux | ✅ | ✅ |
| 端口占用 | `blade create network port occupy` | TCP listen 占用端口，恢复即释放 | Linux / Windows | ✅ | ✅ |
| Pod 删除（自愈） | `blade create k8s pod-kill` | `kubectl delete pod`，恢复即 Kubernetes 自愈（等待替换 Pod Ready） | 任意（kubectl） | ✅ | ✅ |
| 数据库实例 | `blade create mysql` | mysql CLI 注入：连接池占用（SLEEP 会话）/ 表锁持有 / 会话 KILL；密码走 `MYSQL_PWD` 环境变量，零泄露 | 任意（mysql client） | ✅ | ✅ |
| IO 延迟 | — | 计划中（`ioping` / dm-delay） | Linux | ⏳ | ⏳ |

## 如何运行

### Python 版

```bash
cd python
pip install -e ".[dev]"        # 可选：安装后可直接用 chaos-injector 命令

# 查看支持的故障原子
python -m chaos_injector.chaos --list

# 预检（不注入任何东西）
python -m chaos_injector.chaos cpu --load 80 --duration 10 --dry-run

# CPU 打满 1 核至 80%，15 秒后自动恢复
python -m chaos_injector.chaos cpu --load 80 --cores 1 --duration 15 --confirm

# 占用 256MB 内存（Linux 下自动按可用内存做 OOM 保护），15 秒后自动恢复
python -m chaos_injector.chaos mem --size-mb 256 --duration 15 --confirm

# 终止 comm/cmdline 匹配 sleep 的进程（保护自身与祖先链），瞬时故障
python -m chaos_injector.chaos process --pattern sleep --confirm

# 占用 8080 端口（模拟服务不可达），15 秒后自动恢复
python -m chaos_injector.chaos port --port 8080 --duration 15 --confirm

# 数据库故障（三模式）：占满连接池 / 持有表锁 / 杀死匹配会话；密码走 MYSQL_PWD
python -m chaos_injector.chaos mysql --host 127.0.0.1 --user root --password PASS --mode connection --connections 20 --duration 15 --confirm
python -m chaos_injector.chaos mysql --user root --password PASS --database app --mode lock --table orders --duration 15 --confirm
python -m chaos_injector.chaos mysql --user root --password PASS --mode session --session-pattern SLEEP --confirm

# 注入 200ms 网络延迟 + 5% 丢包（Linux 需 root），15 秒后自动恢复
sudo python -m chaos_injector.chaos network --iface eth0 --delay 200 --loss 5 --duration 15 --confirm

# 随机编排：随机选择故障与参数，注入前做负载熔断检查
python -m chaos_injector.chaos random --duration 15 --confirm

# 固定随机种子：同一种子 -> 同一故障（与 Go 实现序列完全一致）
python -m chaos_injector.chaos random --seed 42 --duration 15 --confirm

# 留存实验时间线证据（JSON，含随机种子与注入前后系统快照）
python -m chaos_injector.chaos cpu --load 80 --duration 15 --confirm --timeline artifacts/exp.json
```

### Go 版

```bash
cd go
go build -o chaos-injector ./cmd/chaos-injector

# 查看支持的故障原子
./chaos-injector --list

# 预检（不注入任何东西）
./chaos-injector cpu -duration 10 -load 80 -dry-run

# CPU 打满 1 核至 80%，15 秒后自动恢复
./chaos-injector cpu -duration 15 -load 80 -cores 1 -confirm

# 占用 256MB 内存（Linux 下 OOM 保护），15 秒后自动恢复
./chaos-injector mem -size-mb 256 -duration 15 -confirm

# 终止匹配 sleep 的进程（保护自身与祖先链）
./chaos-injector process -pattern sleep -confirm

# 占用 8080 端口，15 秒后自动恢复
./chaos-injector port -port 8080 -duration 15 -confirm

# 数据库故障（三模式）：连接池占用 / 表锁持有 / 会话 KILL；密码走 MYSQL_PWD
./chaos-injector mysql -host 127.0.0.1 -user root -password PASS -mode connection -connections 20 -duration 15 -confirm
./chaos-injector mysql -user root -password PASS -database app -mode lock -table orders -duration 15 -confirm
./chaos-injector mysql -user root -password PASS -mode session -session-pattern SLEEP -confirm

# 网络延迟 + 丢包（Linux 需 root）
sudo ./chaos-injector network -iface eth0 -delay 200 -loss 5 -duration 15 -confirm

# 随机编排（含负载熔断检查）
./chaos-injector random -duration 15 -confirm

# 固定随机种子：同一种子 -> 同一故障（与 Python 实现序列完全一致）
./chaos-injector random -seed 42 -duration 15 -confirm

# 留存实验时间线证据（JSON）
./chaos-injector cpu -duration 15 -load 80 -confirm -timeline ../artifacts/go-exp.json
```

### Kubernetes 云原生注入（k8s 子命令）

> 从 v0.3 起支持云原生场景：**编排器在本地，故障注入到目标 Pod 内部**，与
> chaosblade 的 k8s 模式 / chaos-mesh 的 chaos-daemon 同构，实现真实混沌工程闭环。

**工作模型**：编排器（Python 或 Go）通过 `kubectl`（或 `minikube kubectl --` 回退）操作集群；
`exec` 子命令把 Linux 版 Go 单二进制（静态编译，不依赖 Pod 内的解释器）用 `kubectl cp` 送进 Pod，
在 Pod 内运行既有故障原子；**Pod 侧代理自带自动回滚定时器**，编排器即使中途崩溃也会恢复。
`pod-kill` 原子删除 Pod 后不回滚——恢复就是 Kubernetes 自愈（Deployment 重建 + Ready），
`--wait-ready` 把实验变成一次自愈验证。

**前置条件**：

- `kubectl` 可用（或 minikube），目标命名空间 / 选择器存在；
- `exec` 需要本地有 `GOOS=linux` 编译的 Go 二进制（`cd go && GOOS=linux go build -o ../artifacts/chaos-injector-linux-amd64 ./cmd/chaos-injector`）；
- `kubectl cp` 依赖 Pod 内有 `tar`（busybox 自带）；`network` 原子要求 Pod 以 privileged 运行（CAP_NET_ADMIN）
  且镜像内含 `tc`（如 `nicolaka/netshoot`）；cpu / mem / process / port 无特权要求；
  `mysql` 原子要求 Pod 内含 `mysql` 客户端（如 `mysql:8` 镜像）并可连通目标数据库。

Go 版（编排器标志在原子名之前，原子后的标志原样传给 Pod 内代理——双重 `-confirm` 闸门）：

```bash
cd go && ./chaos-injector k8s --help

# pod-kill 自愈验证：删除 1 个匹配 Pod，等待替换 Pod Ready（最多 90s）
./chaos-injector k8s pod-kill -selector app=chaos-demo -wait-ready 90 -confirm -timeline ../artifacts/minikube/k8s/podkill.json

# 在 Pod 内注入 CPU 故障 4 秒（代理自动回滚，时间线拷回本地）
./chaos-injector k8s exec -pod chaos-demo-xxx -agent ../artifacts/chaos-injector-linux-amd64 -confirm cpu -confirm -duration 4 -timeline tmp/k8s-cpu.json

# 在特权 Pod 内注入 100ms 网络延迟（RTT 实测 0.04ms → 100.06ms → 0.03ms）
./chaos-injector k8s exec -pod chaos-net -agent ../artifacts/chaos-injector-linux-amd64 -confirm network -confirm -iface eth0 -delay 100 -duration 12 -timeline tmp/k8s-net.json
```

Python 版（语义一致）：

```bash
cd python && PYTHONPATH=src python -m chaos_injector.chaos k8s pod-kill --selector app=chaos-demo --wait-ready 60 --confirm --timeline ../artifacts/minikube/k8s/podkill.json
PYTHONPATH=src python -m chaos_injector.chaos k8s exec --pod chaos-demo-xxx --agent ../artifacts/chaos-injector-linux-amd64 --confirm cpu -confirm -duration 4 -timeline tmp/k8s-cpu.json
```

## 安全设计

- 真实注入必须显式传入 `--confirm` / `-confirm`，否则拒绝执行（exit 2）
- 每条故障都有**幂等的恢复接口**：回滚失败不会抛出异常，重复回滚安全（`tc` 的"已不存在"rc=2 视为成功）
- 三重回滚保障：定时器自动回滚（`threading.Timer` / `time.AfterFunc`）→ 显式 recover（Ctrl-C/异常兜底）→ 进程退出即清理
- 随机模式注入前检查系统负载，**过载主机拒绝注入**（熔断）
- `random --seed N` 双端共享同一 PRNG（xorshift64*）：同种子产生相同故障与参数，**Python/Go 序列完全一致**；
  未传 seed 时自动生成并打印，时间线 JSON 记录 `seed` 与注入前后系统快照（hostname / loadavg / 可用内存），
  任何实验都可事后回放（稳定复现）
- 双实现行为对齐：Go 版时间线事件（start/check/inject/armed/recover/auto/done）与 Python 版完全一致
- 进程故障排除自身与祖先链（`/proc/<pid>/status` PPid 回溯）；内存故障在 Linux 下拒绝超过可用内存的注入（防 OOM）

## 实机验证（minikube）

在 minikube（VirtualBox 驱动，2 核 / 5.9GB 内存）上完成全部 6 个故障原子的真实注入验证，
证据 JSON 留存于 `artifacts/minikube/`：

| 原子 | 验证方法 | 结果 |
|---|---|---|
| network | `ping` 往返时延 0.28ms → 100.18ms → 0.24ms，qdisc 恢复后清空 | net-exp.json |
| cpu | top CPU 占用 0% → 80% → 0% | cpu-exp.json |
| mem | `free` 已用内存 1835MB → 2089MB（≈ +254MB）→ 1825MB | mem-exp.json |
| port | `/dev/tcp` 探测 127.0.0.1:8099：CLOSED → OPEN → CLOSED | port-exp.json |
| process | 后台 `sleep 1000` 被 SIGKILL（TARGET_KILLED），注入器自身存活 | proc-exp.json |
| random | 负载熔断（LoadGate）真实生效 | rand-exp.json |

#### Kubernetes 注入验证（v0.3）

minikube 上完成 8 项实机验证（pod-kill 自愈 ×2 + Pod 内 5 原子 ×2 语言），
证据留存于 `artifacts/minikube/k8s/`：

| 场景 | 验证方法 | 结果 |
|---|---|---|
| pod-kill 自愈（Go） | 删除 Deployment Pod，等待替换 | 替换 Pod Ready，self-healing observed |
| Pod 内 CPU（Go） | 代理注入 80%×1 核 4s | 全生命周期 + 自动回滚 |
| Pod 内内存（Go） | 64MB 占用 4s | 自动回滚 |
| Pod 内端口（Go） | 8080 占用 4s | 自动回滚 |
| Pod 内进程（Go） | 杀死 Pod 内 `sleep 9999` | pgrep 前后对比：存活 → 被 SIGKILL |
| Pod 内网络（Go，特权） | 100ms 延迟 | ping RTT 0.04ms → 100.06ms → 0.03ms |
| pod-kill 自愈（Python） | 同上 | 替换 Pod Ready |
| Pod 内 CPU（Python） | 代理注入 3s | 自动回滚 |

时间线 JSON 逐一对应：`k8s-podkill / k8s-cpu / k8s-mem / k8s-port / k8s-proc / k8s-net / python-podkill / python-cpu .json`。

#### 数据库实例注入验证（v0.4，mysql 原子）

minikube 上部署 MySQL 8（`mysql:8` Pod），通过 `k8s exec` 在 Pod 内注入三种数据库故障，
证据留存于 `artifacts/minikube/k8s/`：

| 模式 | 验证方法 | 结果 |
|---|---|---|
| connection | 占用 5 个连接 25s，注入前后查 `Threads_connected` | 1 → 6（5 个 `SELECT SLEEP(25)` holder）→ 自动恢复回 1；mysql-conn.json |
| lock | 持有 `app.orders` 表写锁 30s，期间触发并发 INSERT | INSERT 阻塞于 `Waiting for table metadata lock`，锁释放后成功；mysql-lock.json |
| session | 后台 `SELECT SLEEP(60)` 受害会话，按 SQL 模式匹配 KILL | 受害会话立即中断（ERROR 2013），processlist 无残留；mysql-session.json |

## 项目结构

```
chaos-injector/
├── python/                        # Python 实现（零第三方依赖）
│   ├── pyproject.toml             # src 布局 + pytest/ruff 配置
│   ├── src/chaos_injector/
│   │   ├── faults.py              # 故障执行层 + 环境感知（tc netem / CPU 烧核）
│   │   ├── scheduler.py           # 恢复监控层（threading.Timer 自动回滚 + 时间线）
│   │   ├── k8s.py                 # Kubernetes 编排层（kubectl 封装 + Pod 内代理注入）
│   │   └── chaos.py               # 随机决策层 + CLI 入口
│   └── tests/
│       └── test_chaos_injector.py
├── go/                            # Go 实现（标准库，单二进制）
│   ├── cmd/chaos-injector/
│   │   └── main.go                # CLI：network / cpu / mem / process / port / pod-kill / exec / random
│   └── internal/
│       ├── fault/                 # 故障原子 + 注册表（fault / network / cpu / mem / process / port / podkill / env）
│       ├── k8s/                   # Kubernetes 编排：kubectl 客户端 + Pod 内代理编排（client.go / exec.go）
│       └── experiment/            # 实验编排：AfterFunc 自动回滚 + JSON 时间线
├── docs/
│   └── architecture.md            # 四层架构与故障生命周期
└── Makefile                       # py-test / py-lint / go-test / go-build
```

## 开发

```bash
make py-lint py-test      # Python：ruff + pytest（也可 cd python 后单独执行）
make go-fmt go-vet go-test go-build   # Go：fmt + vet + test + build
```

CI（`.github/workflows/ci.yml`）在 ubuntu-latest 上分别跑 Python（ruff + pytest）与 Go（vet + test + build）两个 job。

## 后续计划

- [ ] Go 版基于 netlink 直操作内核，去掉 shell 依赖
- [ ] 混沌实验 CRD / 控制器化（对标 chaos-mesh），支持定时任务与故障编排
- [ ] 对接 Prometheus 实现故障注入后的可观测性
- [ ] 新增 IO 延迟故障原子（`ioping` / dm-delay）
- [ ] 随机编排引擎升级：接入故障概率分布，模拟真实世界不可预测的故障场景

# chaos-injector (Python edition)

Python 原型实现：故障执行（`tc netem` / CPU 烧核 / 内存占用 / 进程终止 / 端口占用）、
Kubernetes 编排（k8s 子命令：pod-kill 自愈 + Pod 内代理注入）、随机编排、
`threading.Timer` 自动回滚 + JSON 时间线证据。

完整文档见 [../README.md](../README.md)。

```bash
# 开发安装
pip install -e ".[dev]"

# 查看故障原子
python -m chaos_injector.chaos --list

# CPU 打满 1 核至 80%，15 秒后自动恢复
python -m chaos_injector.chaos cpu --load 80 --cores 1 --duration 15 --confirm

# 内存占用 256MB / 端口占用 8080 / 终止匹配 sleep 的进程
python -m chaos_injector.chaos mem --size-mb 256 --duration 15 --confirm
python -m chaos_injector.chaos port --port 8080 --duration 15 --confirm
python -m chaos_injector.chaos process --pattern sleep --confirm

# 随机编排
python -m chaos_injector.chaos random --duration 15 --confirm

# Kubernetes：pod-kill 自愈验证（需 kubectl / minikube）
python -m chaos_injector.chaos k8s pod-kill --selector app=chaos-demo --wait-ready 60 --confirm

# Kubernetes：在 Pod 内注入 CPU 故障（代理 = Linux 版 Go 二进制）
python -m chaos_injector.chaos k8s exec --pod chaos-demo-xxx --agent ../artifacts/chaos-injector-linux-amd64 --confirm cpu -confirm -duration 4
```

## 开发

```bash
ruff check src tests
pytest -q
```

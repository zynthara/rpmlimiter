rpmlimiter —— 支持动态并发与自动调参的 Go RPM 限流器

> 此项目代码基本由AI生成

**概览**
- 基于滑动窗口的严格 RPM（每分钟请求数）限流。
- 可选最大并发控制，用于提供全局背压。
- 基于 P95 延迟的自动并发调参（Auto‑Tuner），运行时自适应调整并发度。
- 动态热切换 RPM 与并发限制，对在途请求零中断。

**安装**
- `go get github.com/zynthara/rpmlimiter`
- Go 版本：`go.mod` 目标为 Go `1.24`。

**快速开始**
- 基础用法
  - `l := rpmlimiter.New(600)`
  - `defer l.Close()`
  - 每次请求前：`rel, err := l.Wait(ctx); if err != nil { /* 处理错误 */ } defer rel()`
- 非阻塞尝试
  - `rel, ok := l.TryAcquire(); if !ok { /* 丢弃或自行排队 */ } else { defer rel() }`

**示例程序**
- 运行：`go run ./cmd/example -h`
- 例子：`go run ./cmd/example -rpm 600 -concurrency 64 -auto -duration 15s -workers 32 -work 15ms -jitter 10ms`

**API**
- 构造
  - `New(rpm int) *RPMLimiter` — 最简方式，仅传入 RPM（其余使用默认值，默认开启自动调参）。
  - `NewWithConfig(cfg Config, auto *AutoTuneConfig) *RPMLimiter` — 完整可配窗口/并发/日志/时钟与自动调参。
- 请求流程
  - `Wait(ctx) (release func(), err error)` — 阻塞直到满足 RPM 与并发条件。
  - `TryAcquire() (release func(), ok bool)` — 立即返回是否获取成功。
  - `WaitWithTimeout(d) (release func(), err error)` — 便捷方法；超时返回 `ErrTimeout`。
- 自省与控制
  - `Available() int` — 当前（同时考虑 RPM 与并发）的可用名额。
  - `GetStats() Stats` — 指标快照。
  - `GetConfigSnapshot() (rpm, maxConc int, window time.Duration)`。
- 动态调参
  - `SetRPM(newRPM int) (old int, err error)` — 运行时安全；上调会按空档唤醒等待者。
  - `SetMaxConcurrency(newMax int) (old int, err error)` — 热切换信号量，不影响在途请求释放。
  - `StartAutoTune(cfg AutoTuneConfig)` / `StopAutoTune()`。
- 关闭
  - `Close()` — 唤醒所有等待者并停止后台协程。

**配置**
- `Config`
  - `RPM`（必填，>0），`MaxConcurrency`（0 表示不限制），`Window`（默认 60s），`ClockFunc`（测试用）。
- `AutoTuneConfig`
  - `Enable`、`AdjustInterval`（建议 3–10s）、`Percentile`（如 0.95）、`SafetyFactor`（1.1–1.5）、
    `MinConcurrency`、`MaxConcurrency`、`SampleSize`（默认 2048）、`SmoothAlpha`（0.1–0.5）、`MaxStepRatio`（<=1）。

**自动调参与算法说明**
- 目标并发：`target = ceil(RPS * p95 * SafetyFactor)`，并采用指数平滑靠拢目标。
- 每步调整受 `MaxStepRatio` 限制以降低抖动，并钳制在 `[MinConcurrency, MaxConcurrency]`。
- 未启用时采样几乎零开销（通过原子标志位快速返回）。

**并发安全**
- 所有对外方法均为并发安全。
- 并发热切换：每次获取记录对应的信号量通道，释放时归还到同一通道；在途请求安全，新的请求使用新通道。

**错误语义**
- `ErrLimiterClosed` — 限流器已关闭。
- `ErrTimeout` — `WaitWithTimeout` 超时。
- `Wait` 会传播 `context` 的错误（如 `context.Canceled`、`context.DeadlineExceeded`）。

**指标语义（Stats）**
- `TotalRequests` — 已通过 RPM 闸的请求数（不包含仍在等待的）。
- `RejectedRequests` — 尝试失败次数（TryAcquire 失败或 Wait 等待期间的取消/超时）。
- `WaitingRequests` — 正在排队等待 RPM 的协程数量。
- `ActiveRequests` — 已过 RPM 且尚未 release 的活跃请求数。

**示例**
- 启用自动调参
  - `cfg := rpmlimiter.Config{RPM: 600, MaxConcurrency: 64, Window: time.Minute}`
  - `auto := rpmlimiter.AutoTuneConfig{Enable: true, AdjustInterval: 5*time.Second, MinConcurrency: 8, MaxConcurrency: 256}`
  - `l := rpmlimiter.New(cfg, &auto)`
  - `defer l.Close()`
  - `rel, err := l.Wait(ctx); if err != nil { /* 处理错误 */ } defer rel()`
- 动态调整
  - `l.SetRPM(900)`；`l.SetMaxConcurrency(128)`

**Makefile**
- `make build` — 编译所有包。
- `make test` — `-race` 下运行测试。
- `make cover` — 生成 `cover.out` 与 `cover.html` 覆盖率报告。
- `make gofmt` — 检查（不修改）代码格式。
- `make example ARGS="..."` — 通过 `go run` 运行示例（如：`make example ARGS="-rpm 600 -concurrency 64 -duration 15s -workers 32"`）。
- `make build-example` — 构建示例二进制到 `./bin/rpmlimiter-example`。
- `make run-example ARGS="..."` — 先构建再运行示例二进制。

**在受限环境运行**
- 如果默认构建缓存路径受限，可指定本地缓存：
  - `GOCACHE=$(pwd)/.gocache GOPATH=$(pwd)/.gopath make test`
  - `GOCACHE=$(pwd)/.gocache GOPATH=$(pwd)/.gopath make cover`

**项目文件**
- 代码：`rpmlimiter.go`
- 测试：`rpmlimiter_test.go`
- Makefile：`Makefile`
- 模块：`go.mod`

**License**
- 见 `LICENSE`。

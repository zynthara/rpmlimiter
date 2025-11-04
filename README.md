rpmlimiter — Go RPM limiter with dynamic concurrency and auto‑tuning

[中文文档](README_zh-CN.md)

> Most of this project's code is AI‑generated.

**Overview**
- Sliding‑window RPM limiting with strict per‑minute caps.
- Optional max concurrency to add global backpressure.
- P95 latency–driven auto‑tuner that adjusts concurrency at runtime.
- Hot‑switching of limits (RPM and concurrency) without interrupting in‑flight requests.

**Install**
- `go get github.com/zynthara/rpmlimiter`
- Go version: `go.mod` targets Go `1.24`.

**Quick Start**
- Basic usage
  - `l := rpmlimiter.New(600)`
  - `defer l.Close()`
  - Per request: `rel, err := l.Wait(ctx); if err != nil { /* handle */ } defer rel()`
- Non‑blocking try
  - `rel, ok := l.TryAcquire(); if !ok { /* drop or queue */ } else { defer rel() }`

**Example App**
- Run: `go run ./cmd/example -h`
- Example: `go run ./cmd/example -rpm 600 -concurrency 64 -auto -duration 15s -workers 32 -work 15ms -jitter 10ms`

**API**
- Construction
  - `New(rpm int) *RPMLimiter` — simplest way: just pass RPM, defaults applied (auto‑tune enabled).
  - `NewWithConfig(cfg Config, auto *AutoTuneConfig) *RPMLimiter` — full control over window/concurrency/logger/clock and auto‑tune.
- Request flow
  - `Wait(ctx) (release func(), err error)` — blocks until both RPM and concurrency allow.
  - `TryAcquire() (release func(), ok bool)` — returns immediately.
  - `WaitWithTimeout(d) (release func(), err error)` — convenience; returns `ErrTimeout` on deadline.
- Introspection and control
  - `Available() int` — instantaneous available slots (RPM and concurrency combined).
  - `GetStats() Stats` — snapshot of counters.
  - `GetConfigSnapshot() (rpm, maxConc int, window time.Duration)`.
- Dynamic tuning
  - `SetRPM(newRPM int) (old int, err error)` — safe at runtime; wakes waiters if capacity increases.
  - `SetMaxConcurrency(newMax int) (old int, err error)` — hot‑switch semaphore without disrupting inflight.
  - `SetMinConcurrency(newMin int) (old int, err error)` — set auto‑tuner lower bound; bumps current cap up to `newMin` if lower (non‑zero cap).
  - `StartAutoTune(cfg AutoTuneConfig)` / `StopAutoTune()`.
- Shutdown
  - `Close()` — wakes all waiters and stops background goroutines.

**Configuration**
- `Config`
  - `RPM` (required, >0), `MaxConcurrency` (0 = unlimited), `Window` (default 60s), `ClockFunc` (for tests).
- `AutoTuneConfig`
  - `Enable`, `AdjustInterval` (3–10s recommended), `Percentile` (e.g., 0.95), `SafetyFactor` (1.1–1.5),
    `MinConcurrency`, `MaxConcurrency`, `SampleSize` (default 2048), `SmoothAlpha` (0.1–0.5), `MaxStepRatio` (<=1).

**Auto‑Tuning Notes**
- Computes `target = ceil(RPS * p95 * SafetyFactor)` and smooths toward it.
- Limits step size (`MaxStepRatio`) to avoid oscillation; clamps to `[MinConcurrency, MaxConcurrency]`.
- Sampling overhead is negligible when disabled (guarded by an atomic flag).

**Thread Safety**
- All public methods are safe for concurrent use.
- Concurrency hot‑switch uses per‑acquisition release channels so in‑flight requests safely release into the
  semaphore they acquired; new requests use the new semaphore.

**Errors**
- `ErrLimiterClosed` — limiter closed.
- `ErrTimeout` — `WaitWithTimeout` deadline exceeded.
- `Wait` propagates `context` errors (e.g., `context.Canceled`, `context.DeadlineExceeded`).

**Stats Semantics**
- `TotalRequests` — count of requests that passed the RPM gate (excludes ones still queued by RPM).
- `RejectedRequests` — failed attempts (immediate RPM rejects or canceled/timeout while waiting).
- `WaitingRequests` — current number of goroutines queued for RPM capacity.
- `ActiveRequests` — currently active (passed RPM, not yet released).
 - `RPM` — current configured requests‑per‑minute limit.
 - `WindowCount` — number of requests currently counted in the sliding time window (used slots).

**Examples**
- With auto‑tune
  - `cfg := rpmlimiter.Config{RPM: 600, MaxConcurrency: 64, Window: time.Minute}`
  - `auto := rpmlimiter.AutoTuneConfig{Enable: true, AdjustInterval: 5*time.Second, MinConcurrency: 8, MaxConcurrency: 256}`
  - `l := rpmlimiter.New(cfg, &auto)`
  - `defer l.Close()`
  - `rel, err := l.Wait(ctx); if err != nil { /* handle */ } defer rel()`
- Dynamically adjust
  - `l.SetRPM(900)`; `l.SetMaxConcurrency(128)`

**Makefile**
- `make build` — build all packages.
- `make test` — run tests with `-race`.
- `make cover` — generate `cover.out` and `cover.html`.
- `make gofmt` — check formatting (no write).
- `make example ARGS="..."` — run example via `go run` (e.g., `make example ARGS="-rpm 600 -concurrency 64 -duration 15s -workers 32"`).
- `make build-example` — build example binary to `./bin/rpmlimiter-example`.
- `make run-example ARGS="..."` — build then run the example binary.

**Running In Restricted Environments**
- Set local cache paths when running build/tests if default cache is restricted:
  - `GOCACHE=$(pwd)/.gocache GOPATH=$(pwd)/.gopath make test`
  - `GOCACHE=$(pwd)/.gocache GOPATH=$(pwd)/.gopath make cover`

**Project Files**
- Code: `rpmlimiter.go:1`
- Tests: `rpmlimiter_test.go:1`
- Makefile: `Makefile:1`
- Module: `go.mod:1`

**License**
- See `LICENSE`.

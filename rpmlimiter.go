package rpmlimiter

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// 基础限流默认
	DefaultRPM            = 130
	DefaultMinConcurrency = 8
	DefaultMaxConcurrency = 128
	DefaultWindow         = time.Minute
)

var (
	ErrLimiterClosed = errors.New("rate limiter is closed")
	ErrTimeout       = errors.New("acquire timeout")
)

// Logger 最小日志接口（与标准库 log.Logger 兼容）
type Logger interface {
	Printf(format string, v ...any)
}

// 默认的自动并发调参配置
var DefaultAutoTune = AutoTuneConfig{
	Enable:         true,
	AdjustInterval: 5 * time.Second,
	Percentile:     0.95,
	SafetyFactor:   1.25,
	MinConcurrency: DefaultMinConcurrency,
	MaxConcurrency: DefaultMaxConcurrency,
	SampleSize:     2048,
	SmoothAlpha:    0.3,
	MaxStepRatio:   0.25,
}

// Stats 统计信息
type Stats struct {
    TotalRequests    int64 // 成功通过 RPM 闸的请求数
    RejectedRequests int64 // 被拒绝/超时/取消的尝试次数（TryAcquire失败或Wait因ctx取消）
    WaitingRequests  int   // 当前等待 RPM 的请求数
    ActiveRequests   int   // 当前处于激活态（已过RPM闸，且尚未release）的请求数
    RPM              int   // 当前配置的 RPM 上限
    Concurrency      int   // 当前并发上限（0=不限制）
    WindowCount      int   // 过去一个时间窗口内的请求计数（滑动窗口内的占用数）
}

// Config 基础限流配置
type Config struct {
	RPM            int           // 每分钟请求数限制(>0)
	MaxConcurrency int           // 最大并发数,0 表示不限制
	Window         time.Duration // 时间窗口,默认 60 秒
	ClockFunc      func() time.Time
	Logger         Logger // 可选：日志记录器（nil 表示不记录）
}

// AutoTuneConfig 自动并发调参器配置
type AutoTuneConfig struct {
	Enable         bool          // 是否启用自动调参（StartAutoTune 也可单独开启）
	AdjustInterval time.Duration // 调整周期，建议 3~10s（默认 5s）
	Percentile     float64       // 使用的分位，例如 0.95
	SafetyFactor   float64       // 安全系数，建议 1.1~1.5
	MinConcurrency int           // 并发下限（至少 1）
	MaxConcurrency int           // 并发上限（>= MinConcurrency）
	SampleSize     int           // 采样窗口大小（最近 N 次请求，默认 2048）
	SmoothAlpha    float64       // 平滑系数：new = alpha*target + (1-alpha)*current（0.1~0.5）
	MaxStepRatio   float64       // 每次调整的最大相对步长，例如 0.25 表示最多±25%
}

// RPMLimiter 实现严格的 RPM 控制（滑动窗口）+ 动态并发
type RPMLimiter struct {
	rpm            int
	maxConcurrency int
	window         time.Duration

	mu         sync.Mutex
	timestamps []time.Time // 窗口内请求时间戳（单调递增）
	waitQueue  []chan struct{}

	// 并发槽（热切换）：semaphore 指向当前使用的信号量
	semaphore chan struct{}

	// 关闭控制
	closed   bool
	closedCh chan struct{}

	// 滑动清理
	cleanupTicker *time.Ticker
	cleanupStop   chan struct{}
	cleanupWG     sync.WaitGroup

	// 自动调参
	autoCfg    AutoTuneConfig
	tuneTicker *time.Ticker
	tuneStop   chan struct{}
	tuneWG     sync.WaitGroup
	samples    []int64 // 最近 N 个耗时样本（毫秒）
	sampleIdx  int
	sampleFull bool
	latMu      sync.Mutex

	// 时钟
	clockFunc func() time.Time

	// 指标
	stats Stats

	// 标识自动调参是否启用（避免 observeLatency 读写竞态且更廉价）
	autoEnabled atomic.Bool

	// 日志
	logger Logger
}

// New 按给定 rpm 使用默认参数创建限流器（启用默认 AutoTune）
func New(rpm int) *RPMLimiter {
	return NewWithConfig(
		Config{
			RPM:            rpm,
			MaxConcurrency: DefaultMaxConcurrency,
			Window:         DefaultWindow,
			ClockFunc:      time.Now,
		},
		&DefaultAutoTune,
	)
}

// NewWithConfig 创建新的限流器（可选附带 AutoTuneConfig）
func NewWithConfig(cfg Config, autoCfg *AutoTuneConfig) *RPMLimiter {
	if cfg.RPM <= 0 {
		panic("RPM must be greater than 0")
	}
	if cfg.Window == 0 {
		cfg.Window = time.Minute
	}
	if cfg.ClockFunc == nil {
		cfg.ClockFunc = time.Now
	}

	l := &RPMLimiter{
		rpm:            cfg.RPM,
		maxConcurrency: cfg.MaxConcurrency,
		window:         cfg.Window,
		timestamps:     make([]time.Time, 0, cfg.RPM),
		waitQueue:      make([]chan struct{}, 0),
		clockFunc:      cfg.ClockFunc,
		closedCh:       make(chan struct{}),
		cleanupStop:    make(chan struct{}),
		logger:         cfg.Logger,
	}

	if cfg.MaxConcurrency > 0 {
		l.semaphore = make(chan struct{}, cfg.MaxConcurrency)
	}

	l.startCleanup()

	if autoCfg != nil && autoCfg.Enable {
		l.StartAutoTune(*autoCfg)
	}

	l.logf("init: rpm=%d maxConc=%d window=%s auto=%v", l.rpm, l.maxConcurrency, l.window, autoCfg != nil && autoCfg.Enable)

	return l
}

// acquireState：两阶段释放需要记录“本次获取用的是哪个信号量”
type acquireState struct {
	semCh     chan struct{} // 本次获取使用的信号量（可能是旧/新/或nil）
	activated bool          // 是否已通过 RPM 闸（ActiveRequests++）
	startAt   time.Time     // 通过 RPM 闸的开始时间（用于耗时观测）
}

// Wait 阻塞等待直到可以发起请求；返回 release()，调用方完成后必须调用
func (l *RPMLimiter) Wait(ctx context.Context) (func(), error) {
	st := &acquireState{}

	// 先获取并发槽（提供全局背压）
	sem := l.currentSemaphore()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			st.semCh = sem
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.closedCh:
			return nil, ErrLimiterClosed
		}
	}

	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			l.release(st)
			return nil, ErrLimiterClosed
		}

		now := l.clockFunc()
		l.removeExpiredLocked(now)

		// 是否有 RPM 名额
		if len(l.timestamps) < l.rpm {
			l.timestamps = append(l.timestamps, now)
			l.stats.TotalRequests++
			l.stats.ActiveRequests++
			st.activated = true
			st.startAt = now
			l.mu.Unlock()

			release := func() { l.release(st) }
			return release, nil
		}

		// 无名额：进入等待队列 + 计算最久等到时间（窗口左端+window）
		oldest := l.timestamps[0]
		waitUntil := oldest.Add(l.window)
		waitDur := waitUntil.Sub(now)

		notifyCh := make(chan struct{}, 1)
		l.waitQueue = append(l.waitQueue, notifyCh)
		l.stats.WaitingRequests++
		l.mu.Unlock()

		timer := time.NewTimer(waitDur)
		select {
		case <-notifyCh:
			timer.Stop()
			l.mu.Lock()
			l.stats.WaitingRequests--
			l.mu.Unlock()
			continue
		case <-timer.C:
			l.mu.Lock()
			l.stats.WaitingRequests--
			l.mu.Unlock()
			continue
		case <-ctx.Done():
			timer.Stop()
			l.mu.Lock()
			l.stats.WaitingRequests--
			l.stats.RejectedRequests++
			l.mu.Unlock()
			l.release(st)
			return nil, ctx.Err()
		case <-l.closedCh:
			timer.Stop()
			l.mu.Lock()
			l.stats.WaitingRequests--
			l.mu.Unlock()
			l.release(st)
			return nil, ErrLimiterClosed
		}
	}
}

// TryAcquire 尝试立即获取许可（不阻塞）；成功返回 release()
func (l *RPMLimiter) TryAcquire() (func(), bool) {
	st := &acquireState{}

	// 非阻塞占并发槽
	sem := l.currentSemaphore()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			st.semCh = sem
		default:
			return nil, false
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		l.release(st)
		return nil, false
	}

	now := l.clockFunc()
	l.removeExpiredLocked(now)

	if len(l.timestamps) < l.rpm {
		l.timestamps = append(l.timestamps, now)
		l.stats.TotalRequests++
		l.stats.ActiveRequests++
		st.activated = true
		st.startAt = now
		release := func() { l.release(st) }
		return release, true
	}

	// RPM 不足：拒绝；仅释放并发槽
	l.stats.RejectedRequests++
	l.release(st)
	return nil, false
}

// Available 返回“此刻可发起”的名额（同时考虑 RPM 与并发）
func (l *RPMLimiter) Available() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clockFunc()
	l.removeExpiredLocked(now)

	rpmAvail := l.rpm - len(l.timestamps)
	if rpmAvail < 0 {
		rpmAvail = 0
	}

	if l.semaphore == nil {
		return rpmAvail
	}
	concurrencyAvail := cap(l.semaphore) - len(l.semaphore)
	if concurrencyAvail < 0 {
		concurrencyAvail = 0
	}
	if rpmAvail < concurrencyAvail {
		return rpmAvail
	}
	return concurrencyAvail
}

// GetStats 返回当前统计快照
func (l *RPMLimiter) GetStats() Stats {
    l.mu.Lock()
    defer l.mu.Unlock()
    // 非侵入式计算窗口内计数（不修改 timestamps，避免副作用）
    now := l.clockFunc()
    cutoff := now.Add(-l.window)
    wc := 0
    // timestamps 按时间递增，找到第一个未过期的位置
    i := 0
    for i < len(l.timestamps) && l.timestamps[i].Before(cutoff) {
        i++
    }
    if i < len(l.timestamps) {
        wc = len(l.timestamps) - i
    }

    s := l.stats
    s.RPM = l.rpm
    s.Concurrency = l.maxConcurrency
    s.WindowCount = wc
    return s
}

// SetRPM 动态设置 RPM（>0）；提升 RPM 会按“当前空档”唤醒等待者
func (l *RPMLimiter) SetRPM(newRPM int) (old int, err error) {
	if newRPM <= 0 {
		return 0, errors.New("SetRPM: rpm must be > 0")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return l.rpm, ErrLimiterClosed
	}

	old = l.rpm
	if newRPM == old {
		return old, nil
	}

	now := l.clockFunc()
	l.removeExpiredLocked(now)

	l.rpm = newRPM
	changed := l.rpm != old

	// 提升 RPM：尝试立刻唤醒等待者
	if l.rpm > old {
		gap := l.rpm - len(l.timestamps)
		if gap > 0 {
			if gap > len(l.waitQueue) {
				gap = len(l.waitQueue)
			}
			for i := 0; i < gap; i++ {
				select {
				case l.waitQueue[i] <- struct{}{}:
				default:
					close(l.waitQueue[i]) // 兜底唤醒
				}
			}
			l.waitQueue = l.waitQueue[gap:]
		}
	}

	// 解锁后记录日志
	if changed {
		l.logf("set rpm: %d -> %d", old, newRPM)
	}
	return old, nil
}

// SetMaxConcurrency 动态设置最大并发（0=不限制）：热切换信号量
func (l *RPMLimiter) SetMaxConcurrency(newMax int) (old int, err error) {
	if newMax < 0 {
		return 0, errors.New("SetMaxConcurrency: max must be >= 0")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return l.maxConcurrency, ErrLimiterClosed
	}

	old = l.maxConcurrency
	if newMax == old {
		return old, nil
	}

	l.maxConcurrency = newMax
	changed := l.maxConcurrency != old

	if newMax == 0 {
		l.semaphore = nil
	} else {
		l.semaphore = make(chan struct{}, newMax)
	}

	// 解锁后记录日志
	if changed {
		l.logf("set maxConcurrency: %d -> %d", old, newMax)
	}
	return old, nil
}

// SetMinConcurrency 动态设置自动调参的并发下限（>=1）。
// - 若当前并发上限（maxConcurrency）小于新下限，则会立即将并发上限提升到 newMin，
//   以确保实际并发能力不低于该下限（不影响已在进行中的请求）。
// - 若传入 0 或负数，返回错误。
// - 若限流器已关闭，返回 ErrLimiterClosed。
func (l *RPMLimiter) SetMinConcurrency(newMin int) (old int, err error) {
    if newMin <= 0 {
        return 0, errors.New("SetMinConcurrency: min must be >= 1")
    }

    l.mu.Lock()
    if l.closed {
        old = l.autoCfg.MinConcurrency
        l.mu.Unlock()
        return old, ErrLimiterClosed
    }

    old = l.autoCfg.MinConcurrency
    // 更新下限，并确保上限不小于下限
    l.autoCfg.MinConcurrency = newMin
    if l.autoCfg.MaxConcurrency < newMin {
        l.autoCfg.MaxConcurrency = newMin
    }

    // 如当前并发上限低于新下限且非“无限制(0)”场景，则需要在锁外进行提升
    bumpNeeded := l.maxConcurrency > 0 && l.maxConcurrency < newMin
    l.mu.Unlock()

    if bumpNeeded {
        if _, err := l.SetMaxConcurrency(newMin); err != nil {
            return old, err
        }
    }

    if old != newMin {
        l.logf("set minConcurrency: %d -> %d", old, newMin)
    }
    return old, nil
}

// GetConfigSnapshot 返回当前（快照）配置
func (l *RPMLimiter) GetConfigSnapshot() (rpm int, maxConc int, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rpm, l.maxConcurrency, l.window
}

// Close 关闭限流器，唤醒所有等待者并停止后台任务
func (l *RPMLimiter) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	close(l.closedCh)

	// 唤醒所有等 RPM 的等待者
	for _, ch := range l.waitQueue {
		close(ch)
	}
	l.waitQueue = nil
	l.mu.Unlock()

	// 停止清理协程
	if l.cleanupTicker != nil {
		l.cleanupTicker.Stop()
	}
	close(l.cleanupStop)
	l.cleanupWG.Wait()

	// 停止自动调参
	l.StopAutoTune()

	l.logf("closed")
}

// WaitWithTimeout 带超时的等待
func (l *RPMLimiter) WaitWithTimeout(timeout time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rel, err := l.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrTimeout
	}
	return rel, err
}

// ---------- 自动并发调参器（Auto-Tuner） ----------

// StartAutoTune 按配置启动自动调参器
func (l *RPMLimiter) StartAutoTune(cfg AutoTuneConfig) {
	// 填默认值
	if cfg.AdjustInterval <= 0 {
		cfg.AdjustInterval = 5 * time.Second
	}
	if cfg.Percentile <= 0 || cfg.Percentile >= 1 {
		cfg.Percentile = 0.95
	}
	if cfg.SafetyFactor <= 0 {
		cfg.SafetyFactor = 1.25
	}
	if cfg.MinConcurrency <= 0 {
		cfg.MinConcurrency = 1
	}
	if cfg.MaxConcurrency < cfg.MinConcurrency {
		cfg.MaxConcurrency = cfg.MinConcurrency
	}
	if cfg.SampleSize <= 0 {
		cfg.SampleSize = 2048
	}
	if cfg.SmoothAlpha <= 0 || cfg.SmoothAlpha > 1 {
		cfg.SmoothAlpha = 0.3
	}
	if cfg.MaxStepRatio <= 0 || cfg.MaxStepRatio > 1 {
		cfg.MaxStepRatio = 0.25
	}

	l.mu.Lock()
	if l.tuneTicker != nil {
		// 已在运行，覆盖配置
		l.autoCfg = cfg
		l.mu.Unlock()
		return
	}
	l.autoCfg = cfg
	l.tuneStop = make(chan struct{})
	l.tuneTicker = time.NewTicker(cfg.AdjustInterval)

	// 初始化样本缓冲
	l.latMu.Lock()
	l.samples = make([]int64, cfg.SampleSize)
	l.sampleIdx = 0
	l.sampleFull = false
	l.latMu.Unlock()
	// 样本缓冲就绪后，再开启标志位，避免 observeLatency 并发访问未初始化样本
	l.autoEnabled.Store(true)
	l.mu.Unlock()

	l.tuneWG.Add(1)
	go l.runAutoTune()

	l.logf("auto-tune start: interval=%s p=%.2f safety=%.2f min=%d max=%d sample=%d alpha=%.2f step=%.2f",
		l.autoCfg.AdjustInterval, l.autoCfg.Percentile, l.autoCfg.SafetyFactor, l.autoCfg.MinConcurrency,
		l.autoCfg.MaxConcurrency, l.autoCfg.SampleSize, l.autoCfg.SmoothAlpha, l.autoCfg.MaxStepRatio)
}

// StopAutoTune 停止自动调参器（若未开启则无操作）
func (l *RPMLimiter) StopAutoTune() {
	l.mu.Lock()
	ticker := l.tuneTicker
	stopCh := l.tuneStop
	if ticker == nil {
		l.mu.Unlock()
		return
	}
	l.tuneTicker = nil
	l.tuneStop = nil
	// 先关闭启用标志，避免 observeLatency 在关闭过程中的竞态
	l.autoEnabled.Store(false)
	l.mu.Unlock()

	ticker.Stop()
	close(stopCh)
	l.tuneWG.Wait()

	l.logf("auto-tune stop")
}

// runAutoTune 核心循环：计算 p95 → 目标并发 → 平滑/钳制 → SetMaxConcurrency
func (l *RPMLimiter) runAutoTune() {
	defer l.tuneWG.Done()

	// Capture channels locally to avoid data races on fields during StopAutoTune.
	l.mu.Lock()
	ticker := l.tuneTicker
	stop := l.tuneStop
	l.mu.Unlock()

	for {
		select {
		case <-ticker.C:
			rpm, curConc, _ := l.GetConfigSnapshot()
			if rpm <= 0 {
				continue
			}

			p95Sec, ok := l.currentPxxSeconds()
			if !ok || p95Sec <= 0 {
				// 样本不足：使用保守猜测 1s
				p95Sec = 1.0
			}

			rps := float64(rpm) / 60.0
			target := int(math.Ceil(rps * p95Sec * l.autoCfg.SafetyFactor))
			if target < l.autoCfg.MinConcurrency {
				target = l.autoCfg.MinConcurrency
			}
			if target > l.autoCfg.MaxConcurrency {
				target = l.autoCfg.MaxConcurrency
			}
			if target <= 0 {
				target = 1
			}

			// 平滑：new = alpha*target + (1-alpha)*current
			suggest := int(math.Round(l.autoCfg.SmoothAlpha*float64(target) + (1.0-l.autoCfg.SmoothAlpha)*float64(max(curConc, 1))))

			// 限制每步最大变化比例，避免抖动
			maxUp := int(math.Ceil((1.0 + l.autoCfg.MaxStepRatio) * float64(max(curConc, 1))))
			minDown := int(math.Floor((1.0 - l.autoCfg.MaxStepRatio) * float64(max(curConc, 1))))
			if suggest > maxUp {
				suggest = maxUp
			}
			if suggest < minDown {
				suggest = minDown
			}

			// 钳制到范围
			if suggest < l.autoCfg.MinConcurrency {
				suggest = l.autoCfg.MinConcurrency
			}
			if suggest > l.autoCfg.MaxConcurrency {
				suggest = l.autoCfg.MaxConcurrency
			}

			if suggest != curConc {
				_, _ = l.SetMaxConcurrency(suggest)
				l.logf("auto-tune adjust: rpm=%d p95=%.3fs target=%d suggest=%d prev=%d", rpm, p95Sec, target, suggest, curConc)
			}
		case <-l.closedCh:
			return
		case <-stop:
			return
		}
	}
}

// currentPxxSeconds 读取样本并计算当前 pxx（单位：秒）
func (l *RPMLimiter) currentPxxSeconds() (float64, bool) {
	l.latMu.Lock()
	var n int
	if l.sampleFull {
		n = len(l.samples)
	} else {
		n = l.sampleIdx
	}
	if n == 0 {
		l.latMu.Unlock()
		return 0, false
	}
	tmp := make([]int64, n)
	copy(tmp, l.samples[:n])
	l.latMu.Unlock()

	// 排序后取分位
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	px := l.autoCfg.Percentile
	if px <= 0 || px >= 1 {
		px = 0.95
	}
	// 最近邻索引
	idx := int(math.Floor(px * float64(n-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}

	ms := float64(tmp[idx])
	sec := ms / 1000.0
	return sec, true
}

// observeLatency 记录一次请求的耗时（毫秒）
func (l *RPMLimiter) observeLatency(d time.Duration) {
	if !l.autoEnabled.Load() {
		return
	}
	ms := int64(d / time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	l.latMu.Lock()
	l.samples[l.sampleIdx] = ms
	l.sampleIdx++
	if l.sampleIdx >= len(l.samples) {
		l.sampleIdx = 0
		l.sampleFull = true
	}
	l.latMu.Unlock()
}

// ---------- 内部方法 ----------

func (l *RPMLimiter) currentSemaphore() chan struct{} {
	l.mu.Lock()
	sem := l.semaphore
	l.mu.Unlock()
	return sem
}

func (l *RPMLimiter) release(st *acquireState) {
	// 先更新统计（只在激活态时减少 ActiveRequests）
	if st.activated {
		// 观测耗时
		if !st.startAt.IsZero() {
			now := l.clockFunc()
			l.observeLatency(now.Sub(st.startAt))
		}
		l.mu.Lock()
		l.stats.ActiveRequests--
		l.mu.Unlock()
	}
	// 再释放并发槽到“当次获取的信号量”
	if st.semCh != nil {
		select {
		case <-st.semCh:
		default:
			// 若异常（比如重复释放），忽略
		}
	}
}

// removeExpiredLocked 移除过期时间戳 + 唤醒同等数量等待者（必须在锁内调用）
func (l *RPMLimiter) removeExpiredLocked(now time.Time) {
	cutoff := now.Add(-l.window)

	i := 0
	for i < len(l.timestamps) && l.timestamps[i].Before(cutoff) {
		i++
	}

	if i > 0 {
		l.timestamps = l.timestamps[i:]
	}
	// 尽量填满所有当前可用名额（包括历史遗留的空档），避免利用率不足
	available := l.rpm - len(l.timestamps)
	if available > 0 {
		l.notifyWaitersLocked(available)
	}
}

// notifyWaitersLocked 在锁内调用；最多唤醒 count 个等待者（FIFO）
func (l *RPMLimiter) notifyWaitersLocked(count int) {
	if count <= 0 || len(l.waitQueue) == 0 {
		return
	}
	if count > len(l.waitQueue) {
		count = len(l.waitQueue)
	}

	for i := 0; i < count; i++ {
		select {
		case l.waitQueue[i] <- struct{}{}:
		default:
			close(l.waitQueue[i]) // 兜底唤醒
		}
	}
	l.waitQueue = l.waitQueue[count:]
}

// 启动周期性清理（Ticker 方案）
func (l *RPMLimiter) startCleanup() {
	interval := l.window / 10
	if interval < time.Second {
		interval = time.Second
	}
	l.cleanupTicker = time.NewTicker(interval)

	l.cleanupWG.Add(1)
	go func() {
		defer l.cleanupWG.Done()
		for {
			select {
			case <-l.cleanupTicker.C:
				l.mu.Lock()
				if !l.closed {
					now := l.clockFunc()
					l.removeExpiredLocked(now)
				}
				l.mu.Unlock()
			case <-l.cleanupStop:
				return
			case <-l.closedCh:
				return
			}
		}
	}()
}

// 小工具
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// 日志包装
func (l *RPMLimiter) logf(format string, args ...any) {
	if l.logger != nil {
		l.logger.Printf("[rpmlimiter] "+format, args...)
	}
}

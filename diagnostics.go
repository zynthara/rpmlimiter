package rpmlimiter

import (
    "sync"
    "time"
)

// DiagnosticSnapshot is a single diagnostic observation of limiter state.
// It helps reason about whether RPM is saturated and what the bottleneck is.
type DiagnosticSnapshot struct {
    Timestamp time.Time

    // Throughput
    RPS float64

    // Config + instantaneous window utilization
    RPM         int
    WindowCount int
    // Window utilization within [0,1]; approx how full the sliding minute is.
    RPMUtilization float64

    // Instantaneous state
    Active   int
    Waiting  int // waiting for RPM only
    
    // Concurrency caps and instantaneous availability
    Concurrency           int // 0 means unlimited
    Available             int // min(rpmAvail, concAvail)
    RPMAvailable          int // RPM - WindowCount (>=0)
    ConcurrencyAvailable  int // -1 if unlimited

    // Derived latency estimations
    EstimatedServiceTimeSeconds float64 // ≈ Active / RPS (if RPS>0)
    P95Seconds                  float64 // from internal samples if available
    P95Valid                    bool

    // A simple classification of the apparent bottleneck.
    // Possible values: "rpm", "concurrency", "both(rpm+concurrency)", "underutilized", "unknown".
    Bottleneck string
}

// DiagnosticOptions tunes StartDiagnostics behavior.
type DiagnosticOptions struct {
    // Interval between snapshots. Defaults to 1s if <=0.
    Interval time.Duration

    // OnSnapshot, if set, is invoked for each snapshot. If nil, the limiter's
    // logger (if configured) will log a compact line per snapshot.
    OnSnapshot func(s DiagnosticSnapshot)
}

// StartDiagnostics starts a lightweight goroutine that periodically computes a
// DiagnosticSnapshot and either emits it via opts.OnSnapshot or logs it via the
// limiter's logger (if present). It returns a stop() function to halt sampling.
//
// Typical usage:
//   stop := limiter.StartDiagnostics(rpmlimiter.DiagnosticOptions{Interval: time.Second, OnSnapshot: func(s rpmlimiter.DiagnosticSnapshot){ ... }})
//   defer stop()
func (l *RPMLimiter) StartDiagnostics(opts DiagnosticOptions) (stop func()) {
    interval := opts.Interval
    if interval <= 0 {
        interval = time.Second
    }

    // Prepare initial counters for RPS calculation.
    s0 := l.GetStats()
    prevTotal := s0.TotalRequests
    prevAt := time.Now()

    stopCh := make(chan struct{})
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case now := <-ticker.C:
                snap := l.diagnoseOnce(now, prevTotal, prevAt)
                prevTotal = snapPrevTotal(snap, l)
                prevAt = now
                if opts.OnSnapshot != nil {
                    opts.OnSnapshot(snap)
                } else {
                    // Compact log line via limiter logger (if any)
                    l.logf("diag: rps=%.2f rpm=%d util=%.2f active=%d wait=%d conc=%d avail=%d(rpm=%d conc=%d) W≈%.3fs p95=%.3fs*%v bottleneck=%s",
                        snap.RPS, snap.RPM, snap.RPMUtilization, snap.Active, snap.Waiting,
                        snap.Concurrency, snap.Available, snap.RPMAvailable, snap.ConcurrencyAvailable,
                        snap.EstimatedServiceTimeSeconds, snap.P95Seconds, snap.P95Valid, snap.Bottleneck)
                }
            case <-stopCh:
                return
            case <-l.closedCh:
                return
            }
        }
    }()

    return func() {
        close(stopCh)
        wg.Wait()
    }
}

// DiagnoseOnce computes a single snapshot immediately. It is safe for external callers.
func (l *RPMLimiter) DiagnoseOnce() DiagnosticSnapshot {
    // Use now and infer RPS from a minimal interval of 1s with zero delta when
    // called standalone (RPS will show as 0 until a second call by user code).
    s := l.GetStats()
    return l.diagnoseOnce(time.Now(), s.TotalRequests, time.Now().Add(-time.Second))
}

// diagnoseOnce builds a snapshot at a given time using previous counters.
func (l *RPMLimiter) diagnoseOnce(now time.Time, prevTotal int64, prevAt time.Time) DiagnosticSnapshot {
    s := l.GetStats()

    // Compute RPS from cumulative totals.
    dt := now.Sub(prevAt).Seconds()
    dTotal := float64(s.TotalRequests - prevTotal)
    rps := 0.0
    if dt > 0 && dTotal >= 0 {
        rps = dTotal / dt
    }

    // Availability breakdowns
    rpmAvail := s.RPM - s.WindowCount
    if rpmAvail < 0 {
        rpmAvail = 0
    }
    concAvail := l.currentConcurrencyAvail()
    combinedAvail := l.Available()

    // Latency estimates
    estW := 0.0
    if rps > 0 {
        estW = float64(s.ActiveRequests) / rps
    }
    p95, ok := l.currentPxxSeconds()

    // Bottleneck classification
    rpmSat := s.RPM > 0 && rpmAvail == 0
    concSat := s.Concurrency > 0 && concAvail == 0
    bottleneck := "unknown"
    switch {
    case rpmSat && concSat:
        bottleneck = "both(rpm+concurrency)"
    case rpmSat:
        bottleneck = "rpm"
    case concSat:
        bottleneck = "concurrency"
    default:
        if combinedAvail > 0 && s.WaitingRequests == 0 {
            bottleneck = "underutilized"
        } else {
            bottleneck = "unknown"
        }
    }

    util := 0.0
    if s.RPM > 0 {
        util = float64(s.WindowCount) / float64(s.RPM)
        if util < 0 {
            util = 0
        }
        if util > 1 {
            util = 1
        }
    }

    return DiagnosticSnapshot{
        Timestamp: now,
        RPS:       rps,
        RPM:       s.RPM,
        WindowCount:     s.WindowCount,
        RPMUtilization:  util,
        Active:          s.ActiveRequests,
        Waiting:         s.WaitingRequests,
        Concurrency:     s.Concurrency,
        Available:       combinedAvail,
        RPMAvailable:    rpmAvail,
        ConcurrencyAvailable: concAvail,
        EstimatedServiceTimeSeconds: estW,
        P95Seconds:                 p95,
        P95Valid:                   ok,
        Bottleneck:                 bottleneck,
    }
}

// currentConcurrencyAvail returns available slots in the current semaphore.
// Returns -1 if concurrency is unlimited.
func (l *RPMLimiter) currentConcurrencyAvail() int {
    l.mu.Lock()
    defer l.mu.Unlock()
    if l.semaphore == nil {
        return -1
    }
    avail := cap(l.semaphore) - len(l.semaphore)
    if avail < 0 {
        avail = 0
    }
    return avail
}

// snapPrevTotal derives the new prevTotal from the limiter, allowing the
// caller to persist an accurate baseline even if TotalRequests is reset.
func snapPrevTotal(_ DiagnosticSnapshot, l *RPMLimiter) int64 {
    // Prefer reading fresh stats to handle ResetStats() without drifting.
    s := l.GetStats()
    return s.TotalRequests
}


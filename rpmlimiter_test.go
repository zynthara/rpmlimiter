package rpmlimiter

import (
    "context"
    "sync"
    "testing"
    "time"
)

// waitForCondition polls f until it returns true or timeout.
func waitForCondition(timeout time.Duration, f func() bool) bool {
    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        if f() {
            return true
        }
        time.Sleep(2 * time.Millisecond)
    }
    return f()
}

func TestRPMWindowEnforcesLimit(t *testing.T) {
    l := NewWithConfig(Config{RPM: 3, MaxConcurrency: 0, Window: 50 * time.Millisecond, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Consume all RPM slots quickly
    var rels []func()
    for i := 0; i < 3; i++ {
        rel, ok := l.TryAcquire()
        if !ok {
            t.Fatalf("expected TryAcquire #%d to succeed", i+1)
        }
        rels = append(rels, rel)
    }
    // Fourth should fail within the same window
    if _, ok := l.TryAcquire(); ok {
        t.Fatalf("expected fourth TryAcquire to fail within window")
    }

    // Wait for window to pass and the next acquire should succeed
    time.Sleep(60 * time.Millisecond)
    if rel, ok := l.TryAcquire(); !ok {
        t.Fatalf("expected TryAcquire after window to succeed")
    } else {
        rel()
    }

    // release previously acquired (does not affect RPM window but keeps stats clean)
    for _, r := range rels {
        r()
    }
}

func TestRemoveExpiredWakesAvailableSlots(t *testing.T) {
    l := NewWithConfig(Config{RPM: 2, MaxConcurrency: 0, Window: 500 * time.Millisecond, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Fill RPM window
    for i := 0; i < 2; i++ {
        if rel, ok := l.TryAcquire(); !ok {
            t.Fatalf("expected initial TryAcquire #%d to succeed", i+1)
        } else {
            // Keep releases for later to not affect test (RPM is time-based)
            defer rel()
        }
    }

    // Start 3 waiters that will be queued
    type res struct {
        err error
        rel func()
    }
    results := make(chan res, 3)
    ctxs := make([]context.CancelFunc, 0, 3)
    for i := 0; i < 3; i++ {
        ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        ctxs = append(ctxs, cancel)
        go func() {
            rel, err := l.Wait(ctx)
            results <- res{err: err, rel: rel}
        }()
    }

    // Wait until all 3 are known to be queued
    ok := waitForCondition(200*time.Millisecond, func() bool {
        l.mu.Lock()
        defer l.mu.Unlock()
        return len(l.waitQueue) == 3
    })
    if !ok {
        t.Fatalf("waiters did not queue as expected; queued=%d", func() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.waitQueue) }())
    }

    // Manually expire the entire window and trigger wake based on available slots
    l.mu.Lock()
    now := time.Now().Add(l.window * 2)
    l.removeExpiredLocked(now)
    l.mu.Unlock()

    // Exactly RPM=2 should be woken quickly; third should still be blocked
    var got []res
    for i := 0; i < 2; i++ {
        select {
        case r := <-results:
            if r.err != nil || r.rel == nil {
                t.Fatalf("expected waiter to succeed after expiry; err=%v", r.err)
            }
            got = append(got, r)
        case <-time.After(200 * time.Millisecond):
            t.Fatalf("did not receive expected wake #%d", i+1)
        }
    }
    // Ensure no more successes slip through immediately
    select {
    case r := <-results:
        // If received, it must be a still-blocked context eventually timing out or spurious success
        if r.err == nil && r.rel != nil {
            t.Fatalf("unexpected third wake beyond available slots")
        }
    case <-time.After(120 * time.Millisecond):
        // expected: third is still waiting
    }

    // Cleanup any successful acquires
    for _, r := range got {
        r.rel()
    }
    // Cancel outstanding waiter to unblock test
    for _, c := range ctxs {
        c()
    }
    // Drain remaining result(s)
    for i := 0; i < 3-len(got); i++ {
        <-results
    }
}

func TestSetRPMIncreaseWakesWaiters(t *testing.T) {
    l := NewWithConfig(Config{RPM: 1, MaxConcurrency: 0, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Occupy the single RPM slot
    if rel, ok := l.TryAcquire(); !ok {
        t.Fatalf("expected initial TryAcquire to succeed")
    } else {
        defer rel()
    }

    // Queue two waiters
    results := make(chan error, 2)
    for i := 0; i < 2; i++ {
        go func() {
            rel, err := l.Wait(context.Background())
            if err == nil && rel != nil {
                defer rel()
            }
            results <- err
        }()
    }

    // Wait until queue length reaches 2
    ok := waitForCondition(200*time.Millisecond, func() bool {
        l.mu.Lock()
        defer l.mu.Unlock()
        return len(l.waitQueue) >= 2
    })
    if !ok {
        t.Fatalf("waiters did not queue up for SetRPM test")
    }

    // Increase RPM to 3; should wake two waiting goroutines immediately
    if _, err := l.SetRPM(3); err != nil {
        t.Fatalf("SetRPM failed: %v", err)
    }

    for i := 0; i < 2; i++ {
        select {
        case err := <-results:
            if err != nil {
                t.Fatalf("waiter #%d returned error after SetRPM increase: %v", i+1, err)
            }
        case <-time.After(200 * time.Millisecond):
            t.Fatalf("waiter #%d not woken after SetRPM increase", i+1)
        }
    }
}

func TestAutoTuneObserveLatencyToggle(t *testing.T) {
    l := NewWithConfig(Config{RPM: 10, MaxConcurrency: 0, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Without auto-tune, samples should not accumulate and pxx should be unavailable
    for i := 0; i < 3; i++ {
        rel, err := l.Wait(context.Background())
        if err != nil {
            t.Fatalf("Wait failed: %v", err)
        }
        time.Sleep(5 * time.Millisecond)
        rel()
    }
    if _, ok := l.currentPxxSeconds(); ok {
        t.Fatalf("expected no pxx without auto-tune enabled")
    }

    // Enable auto-tune with small sample size for fast tests
    l.StartAutoTune(AutoTuneConfig{Enable: true, AdjustInterval: 200 * time.Millisecond, SampleSize: 8})
    defer l.StopAutoTune()

    for i := 0; i < 6; i++ {
        rel, err := l.Wait(context.Background())
        if err != nil {
            t.Fatalf("Wait failed: %v", err)
        }
        time.Sleep(10 * time.Millisecond)
        rel()
    }

    if p, ok := l.currentPxxSeconds(); !ok || p <= 0 {
        t.Fatalf("expected valid pxx after auto-tune enabled; got ok=%v p=%v", ok, p)
    }
}

func TestHotSwitchMaxConcurrencyNoLeak(t *testing.T) {
    l := NewWithConfig(Config{RPM: 100, MaxConcurrency: 2, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Acquire two concurrent slots
    r1, err := l.Wait(context.Background())
    if err != nil {
        t.Fatalf("Wait #1 failed: %v", err)
    }
    r2, err := l.Wait(context.Background())
    if err != nil {
        t.Fatalf("Wait #2 failed: %v", err)
    }

    // Start a third waiter that will block on old semaphore
    start := make(chan struct{})
    doneThird := make(chan error, 1)
    go func() {
        close(start)
        rel, err := l.Wait(context.Background())
        if err == nil && rel != nil {
            defer rel()
        }
        doneThird <- err
    }()
    <-start

    // Give a moment to ensure the third waiter is blocked
    time.Sleep(20 * time.Millisecond)

    // Hot-switch to larger concurrency; new waiters should use the new semaphore
    if _, err := l.SetMaxConcurrency(4); err != nil {
        t.Fatalf("SetMaxConcurrency failed: %v", err)
    }

    // Start two more waiters; they should proceed quickly
    var wg sync.WaitGroup
    errs := make(chan error, 2)
    for i := 0; i < 2; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            rel, err := l.Wait(context.Background())
            if err == nil && rel != nil {
                defer rel()
            }
            errs <- err
        }()
    }

    for i := 0; i < 2; i++ {
        select {
        case err := <-errs:
            if err != nil {
                t.Fatalf("new waiter #%d failed after hot-switch: %v", i+1, err)
            }
        case <-time.After(200 * time.Millisecond):
            t.Fatalf("new waiter #%d did not proceed after hot-switch", i+1)
        }
    }

    // Release initial two to unblock the third
    r1()
    r2()

    select {
    case err := <-doneThird:
        if err != nil {
            t.Fatalf("third waiter failed after releases: %v", err)
        }
    case <-time.After(300 * time.Millisecond):
        t.Fatalf("third waiter did not complete after releases")
    }
}

func TestSetMinConcurrencyBumpsCurrentCap(t *testing.T) {
    l := NewWithConfig(Config{RPM: 100, MaxConcurrency: 2, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    if _, err := l.SetMinConcurrency(4); err != nil {
        t.Fatalf("SetMinConcurrency failed: %v", err)
    }
    _, conc, _ := l.GetConfigSnapshot()
    if conc != 4 {
        t.Fatalf("expected concurrency bumped to >= min (4); got %d", conc)
    }
}

func TestSetMinConcurrencyRejectsInvalid(t *testing.T) {
    l := NewWithConfig(Config{RPM: 100, MaxConcurrency: 2, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)
    if _, err := l.SetMinConcurrency(0); err == nil {
        t.Fatalf("expected error for SetMinConcurrency(0)")
    }
}

func TestSetMinConcurrencyNoBumpWhenUnlimited(t *testing.T) {
    l := NewWithConfig(Config{RPM: 100, MaxConcurrency: 0, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)
    if _, err := l.SetMinConcurrency(8); err != nil {
        t.Fatalf("SetMinConcurrency failed: %v", err)
    }
    _, conc, _ := l.GetConfigSnapshot()
    if conc != 0 { // 0 means unlimited, should not be reduced/bounded by min
        t.Fatalf("expected concurrency to remain unlimited (0); got %d", conc)
    }
}

func TestGetStatsShowsRPMAndWindowCount(t *testing.T) {
    l := NewWithConfig(Config{RPM: 3, MaxConcurrency: 0, Window: 50 * time.Millisecond, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Acquire two within the window
    var rels []func()
    for i := 0; i < 2; i++ {
        rel, ok := l.TryAcquire()
        if !ok {
            t.Fatalf("expected TryAcquire #%d to succeed", i+1)
        }
        rels = append(rels, rel)
    }

    s := l.GetStats()
    if s.RPM != 3 {
        t.Fatalf("expected GetStats().RPM == 3, got %d", s.RPM)
    }
    if s.WindowCount != 2 {
        t.Fatalf("expected WindowCount == 2, got %d", s.WindowCount)
    }

    // After window passes, WindowCount should drop to 0
    time.Sleep(70 * time.Millisecond)
    s2 := l.GetStats()
    if s2.WindowCount != 0 {
        t.Fatalf("expected WindowCount == 0 after window, got %d", s2.WindowCount)
    }

    // Cleanup
    for _, r := range rels {
        r()
    }
}

func TestGetStatsShowsConcurrency(t *testing.T) {
    l := NewWithConfig(Config{RPM: 10, MaxConcurrency: 5, Window: time.Second, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    s := l.GetStats()
    if s.Concurrency != 5 {
        t.Fatalf("expected Concurrency == 5, got %d", s.Concurrency)
    }

    // Set to unlimited (0) and verify
    if _, err := l.SetMaxConcurrency(0); err != nil {
        t.Fatalf("SetMaxConcurrency(0) failed: %v", err)
    }
    s = l.GetStats()
    if s.Concurrency != 0 {
        t.Fatalf("expected Concurrency == 0 (unlimited), got %d", s.Concurrency)
    }

    // Increase again and verify
    if _, err := l.SetMaxConcurrency(7); err != nil {
        t.Fatalf("SetMaxConcurrency(7) failed: %v", err)
    }
    s = l.GetStats()
    if s.Concurrency != 7 {
        t.Fatalf("expected Concurrency == 7, got %d", s.Concurrency)
    }
}

func TestResetStatsResetsCumulativeOnly(t *testing.T) {
    l := NewWithConfig(Config{RPM: 1, MaxConcurrency: 0, Window: 200 * time.Millisecond, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Successful acquire increments TotalRequests and ActiveRequests
    rel, ok := l.TryAcquire()
    if !ok {
        t.Fatalf("expected first TryAcquire to succeed")
    }
    // Second acquire should be rejected within the same window -> increments RejectedRequests
    if _, ok := l.TryAcquire(); ok {
        t.Fatalf("expected second TryAcquire to fail within window")
    }

    s := l.GetStats()
    if s.TotalRequests != 1 {
        t.Fatalf("pre-reset: expected TotalRequests=1, got %d", s.TotalRequests)
    }
    if s.RejectedRequests < 1 {
        t.Fatalf("pre-reset: expected RejectedRequests>=1, got %d", s.RejectedRequests)
    }
    if s.ActiveRequests != 1 {
        t.Fatalf("pre-reset: expected ActiveRequests=1, got %d", s.ActiveRequests)
    }

    // Reset only cumulative counters
    l.ResetStats()
    s2 := l.GetStats()
    if s2.TotalRequests != 0 || s2.RejectedRequests != 0 {
        t.Fatalf("post-reset: expected totals to be zero; got total=%d rejected=%d", s2.TotalRequests, s2.RejectedRequests)
    }
    // Active should remain unchanged until release
    if s2.ActiveRequests != 1 {
        t.Fatalf("post-reset: expected ActiveRequests to remain 1, got %d", s2.ActiveRequests)
    }

    // Release and Active should drop
    rel()
    s3 := l.GetStats()
    if s3.ActiveRequests != 0 {
        t.Fatalf("after release: expected ActiveRequests=0, got %d", s3.ActiveRequests)
    }
}

func TestCanceledWaitersDoNotBlockRealWaiters(t *testing.T) {
    // This test verifies the bug fix: canceled waiters should not waste wake-up slots
    l := NewWithConfig(Config{RPM: 2, MaxConcurrency: 0, Window: 500 * time.Millisecond, ClockFunc: time.Now}, nil)
    t.Cleanup(l.Close)

    // Fill RPM window
    for i := 0; i < 2; i++ {
        if rel, ok := l.TryAcquire(); !ok {
            t.Fatalf("expected initial TryAcquire #%d to succeed", i+1)
        } else {
            defer rel()
        }
    }

    // Start 5 waiters: 3 will be canceled, 2 will remain waiting
    type res struct {
        err error
        rel func()
        id  int
    }
    results := make(chan res, 5)
    
    // First 3 waiters will be canceled
    canceledCtxs := make([]context.CancelFunc, 0, 3)
    for i := 0; i < 3; i++ {
        ctx, cancel := context.WithCancel(context.Background())
        canceledCtxs = append(canceledCtxs, cancel)
        go func(id int) {
            rel, err := l.Wait(ctx)
            results <- res{err: err, rel: rel, id: id}
        }(i)
    }

    // Last 2 waiters will stay waiting
    for i := 3; i < 5; i++ {
        go func(id int) {
            ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
            defer cancel()
            rel, err := l.Wait(ctx)
            results <- res{err: err, rel: rel, id: id}
        }(i)
    }

    // Wait until all 5 are queued
    ok := waitForCondition(200*time.Millisecond, func() bool {
        l.mu.Lock()
        defer l.mu.Unlock()
        return len(l.waitQueue) == 5
    })
    if !ok {
        t.Fatalf("waiters did not queue as expected; queued=%d", func() int { 
            l.mu.Lock(); defer l.mu.Unlock(); return len(l.waitQueue) 
        }())
    }

    // Cancel the first 3 waiters
    for _, cancel := range canceledCtxs {
        cancel()
    }

    // Wait for canceled waiters to exit
    for i := 0; i < 3; i++ {
        select {
        case r := <-results:
            if r.err == nil {
                t.Fatalf("expected canceled waiter #%d to return error", r.id)
            }
        case <-time.After(200 * time.Millisecond):
            t.Fatalf("canceled waiter #%d did not exit promptly", i)
        }
    }

    // Now expire the window and trigger wake-up
    l.mu.Lock()
    now := time.Now().Add(l.window * 2)
    l.removeExpiredLocked(now)
    l.mu.Unlock()

    // The 2 remaining waiters should be woken up promptly (not blocked by the canceled ones)
    for i := 0; i < 2; i++ {
        select {
        case r := <-results:
            if r.err != nil || r.rel == nil {
                t.Fatalf("expected remaining waiter to succeed after expiry; err=%v", r.err)
            }
            r.rel()
        case <-time.After(200 * time.Millisecond):
            t.Fatalf("remaining waiter #%d not woken after expiry", i)
        }
    }
}

package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	limiter "github.com/zynthara/rpmlimiter"
)

func main() {
	// Flags
	rpm := flag.Int("rpm", limiter.DefaultRPM, "Requests per minute limit")
	maxConc := flag.Int("concurrency", limiter.DefaultMaxConcurrency, "Max concurrency (0=unlimited)")

	// Auto-tune
	auto := flag.Bool("auto", true, "Enable auto-tuning")
	autoInterval := flag.Duration("auto-interval", 5*time.Second, "Auto-tune adjust interval")
	autoPct := flag.Float64("auto-pct", 0.95, "Auto-tune percentile (e.g. 0.95)")
	autoSafety := flag.Float64("auto-safety", 1.25, "Auto-tune safety factor")
	autoMin := flag.Int("auto-min", limiter.DefaultMinConcurrency, "Auto-tune min concurrency")
	autoMax := flag.Int("auto-max", limiter.DefaultMaxConcurrency, "Auto-tune max concurrency")
	autoSample := flag.Int("auto-sample", 2048, "Auto-tune sample size")
	autoAlpha := flag.Float64("auto-alpha", 0.3, "Auto-tune smoothing alpha (0-1]")
	autoStep := flag.Float64("auto-step", 0.25, "Auto-tune max step ratio (0-1]")

	// Workload
	duration := flag.Duration("duration", 10*time.Second, "How long to run the example")
	workers := flag.Int("workers", 16, "Number of request workers")
	reqTimeout := flag.Duration("req-timeout", 0, "Per-request timeout (0=none)")
	tryAcquire := flag.Bool("try", false, "Use TryAcquire instead of Wait")
	workBase := flag.Duration("work", 20*time.Millisecond, "Simulated work duration inside critical section")
	workJitter := flag.Duration("jitter", 10*time.Millisecond, "Additional random jitter up to this duration")
	statsEvery := flag.Duration("stats", 1*time.Second, "Print stats every interval (0=disable)")
	quiet := flag.Bool("quiet", false, "Less verbose output")

	// Diagnostics
	diag := flag.Bool("diag", false, "Enable diagnostics output")
	diagInterval := flag.Duration("diag-interval", time.Second, "Diagnostics interval (e.g. 1s)")

	flag.Parse()

    // Create limiter with the simplest constructor: only RPM required.
    l := limiter.New(*rpm)
    // Concurrency override if provided
    if *maxConc != limiter.DefaultMaxConcurrency {
        _, _ = l.SetMaxConcurrency(*maxConc)
    }
    // Auto-tune toggle / override
    if !*auto {
        l.StopAutoTune()
    } else {
        // If auto params differ from defaults, restart tuner with custom config
        def := limiter.DefaultAutoTune
        if def.AdjustInterval != *autoInterval || def.Percentile != *autoPct || def.SafetyFactor != *autoSafety ||
            def.MinConcurrency != *autoMin || def.MaxConcurrency != *autoMax || def.SampleSize != *autoSample ||
            def.SmoothAlpha != *autoAlpha || def.MaxStepRatio != *autoStep {
            l.StopAutoTune()
            autoCfg := limiter.AutoTuneConfig{
                Enable:         true,
                AdjustInterval: *autoInterval,
                Percentile:     *autoPct,
                SafetyFactor:   *autoSafety,
                MinConcurrency: *autoMin,
                MaxConcurrency: *autoMax,
                SampleSize:     *autoSample,
                SmoothAlpha:    *autoAlpha,
                MaxStepRatio:   *autoStep,
            }
            l.StartAutoTune(autoCfg)
        }
    }
	defer l.Close()

	// Optional diagnostics
	var stopDiag func()
	if *diag {
		stopDiag = l.StartDiagnostics(limiter.DiagnosticOptions{
			Interval: *diagInterval,
			OnSnapshot: func(s limiter.DiagnosticSnapshot) {
				fmt.Printf("diag: rps=%.1f rpm=%d util=%.2f active=%d wait=%d conc=%d avail=%d(rpm=%d conc=%d) W≈%.2fs p95=%.2fs ok=%v bottleneck=%s\n",
					s.RPS, s.RPM, s.RPMUtilization, s.Active, s.Waiting,
					s.Concurrency, s.Available, s.RPMAvailable, s.ConcurrencyAvailable,
					s.EstimatedServiceTimeSeconds, s.P95Seconds, s.P95Valid, s.Bottleneck)
			},
		})
		defer stopDiag()
	}

	// Root context with duration and signal cancel
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if !*quiet {
        fmt.Printf("rpmlimiter example starting: rpm=%d concurrency=%d auto=%v workers=%d try=%v\n",
            *rpm, *maxConc, *auto, *workers, *tryAcquire)
	}

	// Stats ticker
	stopStats := make(chan struct{})
	if *statsEvery > 0 {
		go func() {
			ticker := time.NewTicker(*statsEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
                    s := l.GetStats()
                    avail := l.Available()
                    fmt.Printf("[stats] rpm=%d conc=%d avail=%d window=%d active=%d waiting=%d total=%d rejected=%d\n",
                        s.RPM, s.Concurrency, avail, s.WindowCount, s.ActiveRequests, s.WaitingRequests, s.TotalRequests, s.RejectedRequests)
				case <-stopStats:
					return
				}
			}
		}()
	}

	// Workers
	var wg sync.WaitGroup
	var tryReject uint64
	randSrc := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Acquire
				var (
					rel func()
					ok  bool
					err error
				)
				if *tryAcquire {
					rel, ok = l.TryAcquire()
					if !ok {
						// slight backoff to avoid busy-spin if both RPM and concurrency are saturated
						atomic.AddUint64(&tryReject, 1)
						time.Sleep(2 * time.Millisecond)
						continue
					}
				} else {
					aCtx := ctx
					var cancelReq context.CancelFunc
					if *reqTimeout > 0 {
						aCtx, cancelReq = context.WithTimeout(ctx, *reqTimeout)
					}
					rel, err = l.Wait(aCtx)
					if cancelReq != nil {
						cancelReq()
					}
					if err != nil {
						// timeout/canceled/closed
						continue
					}
				}

				// Simulate work inside the active section (affects auto-tuner latency)
				jitter := time.Duration(0)
				if *workJitter > 0 {
					// random in [0, workJitter]
					jitter = time.Duration(randSrc.Int63n(int64(*workJitter)))
				}
				time.Sleep(*workBase + jitter)
				rel()
			}
		}(i)
	}

	// Wait for completion
	<-ctx.Done()
	close(stopStats)
	wg.Wait()

	// Final stats
    s := l.GetStats()
    if !*quiet {
        fmt.Printf("done. rpm=%d conc=%d window=%d total=%d rejected=%d waiting=%d active=%d tryRejects=%d\n",
            s.RPM, s.Concurrency, s.WindowCount, s.TotalRequests, s.RejectedRequests, s.WaitingRequests, s.ActiveRequests, tryReject)
    }
}

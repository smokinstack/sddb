package dashboard

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// A scan uses a fixed pool of workers rather than one goroutine per address.
func TestScanUsesBoundedWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	base := runtime.NumGoroutine()
	var peak atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if n := int64(runtime.NumGoroutine()); n > peak.Load() {
					peak.Store(n)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// 4,094 loopback addresses on a closed port: every probe fails fast.
	results, err := ScanNetwork(ctx, "127.77.0.0/20", 1, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range results {
	}
	close(stop)

	if extra := peak.Load() - int64(base); extra > 40 {
		t.Fatalf("scan peaked at %d extra goroutines for 4,094 addresses; want a bounded pool", extra)
	}
}

package aznet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSDKWorkloadMeasurement is opt-in because it samples process memory and
// wall-clock latency against local Azurite. Numbers include HTTP/SDK/emulator
// overhead; they are not Azure throughput or billing estimates.
func TestSDKWorkloadMeasurement(t *testing.T) {
	if os.Getenv("AZNET_MEASURE") != "1" {
		t.Skip("set AZNET_MEASURE=1 with Azurite on localhost:10000-10002")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for service, network := range []string{"azblob", "azqueue", "aztable"} {
		for _, concurrency := range []int{1, 4, 16} {
			for _, workload := range []string{"idle", "interactive", "bulk", "slow"} {
				t.Run(fmt.Sprintf("%s/connections%d/%s", network, concurrency, workload), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()
					metrics := NewDefaultMetrics()
					id := strings.ReplaceAll(uuid.NewString(), "-", "")
					opts := []Option{WithContext(ctx), WithMetrics(metrics), WithEndpoints("h"+id, "t"+id), WithPing(0), WithAcceptPoll(time.Millisecond), WithDataPoll(10 * time.Millisecond), WithFastPoll(time.Millisecond)}
					u := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", 10000+service), Path: "/devstoreaccount1", User: url.UserPassword("devstoreaccount1", key)}
					ln, err := Listen(network, u.String(), opts...)
					if err != nil {
						t.Fatal(err)
					}
					l := ln.(*Listener)
					defer func() {
						if err := l.Close(); err != nil {
							t.Error(err)
						}
						cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
						defer stop()
						if err := l.CleanupBootstrap(cleanup); err != nil {
							t.Error(err)
						}
					}()
					address, err := l.ConnectionString()
					if err != nil {
						t.Fatal(err)
					}
					type pair struct{ client, server net.Conn }
					pairs := make([]pair, 0, concurrency)
					for range concurrency {
						type dialResult struct {
							conn net.Conn
							err  error
						}
						dial := make(chan dialResult, 1)
						go func() { c, e := Dial(network, address, opts...); dial <- dialResult{c, e} }()
						server, e := l.Accept()
						result := <-dial
						if e != nil || result.err != nil {
							t.Fatalf("accept/dial: %v / %v", e, result.err)
						}
						pairs = append(pairs, pair{result.conn, server})
						defer result.conn.Close()
					}
					// Exclude session setup and teardown from workload counters and timing.
					requestTotal := func() int64 {
						var total int64
						for _, n := range metrics.RequestCounts() {
							total += n
						}
						return total
					}
					requestsBefore := requestTotal()
					sentBefore, receivedBefore := metrics.GetBytesSent(), metrics.GetBytesReceived()
					runtime.GC()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)
					peak := before.HeapAlloc
					stopSample := make(chan struct{})
					sampleDone := make(chan struct{})
					go func() {
						defer close(sampleDone)
						tick := time.NewTicker(2 * time.Millisecond)
						defer tick.Stop()
						for {
							select {
							case <-stopSample:
								return
							case <-tick.C:
								var sample runtime.MemStats
								runtime.ReadMemStats(&sample)
								peak = max(peak, sample.HeapAlloc)
							}
						}
					}()
					var wg sync.WaitGroup
					type result struct {
						latencies []time.Duration
						bytes     int
						err       error
					}
					results := make(chan result, concurrency)
					started := time.Now()
					for _, p := range pairs {
						wg.Add(1)
						go func() {
							defer wg.Done()
							r := result{}
							size, iterations := 64, 16
							if workload == "bulk" {
								size, iterations = 256<<10, 4
							}
							if workload == "slow" {
								size, iterations = 32<<10, 4
							}
							if workload == "idle" {
								p.server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
								var b [1]byte
								_, e := p.server.Read(b[:])
								p.server.SetReadDeadline(time.Time{})
								if !errors.Is(e, os.ErrDeadlineExceeded) {
									r.err = fmt.Errorf("idle read: %w", e)
								}
								results <- r
								return
							}
							payload := bytes.Repeat([]byte{42}, size)
							received := make([]byte, size)
							for range iterations {
								start := time.Now()
								if _, e := p.client.Write(payload); e != nil {
									r.err = e
									break
								}
								if workload == "slow" {
									for offset := 0; offset < size; offset += 1024 {
										if _, e := io.ReadFull(p.server, received[offset:min(offset+1024, size)]); e != nil {
											r.err = e
											break
										}
										time.Sleep(time.Millisecond)
									}
								} else {
									_, r.err = io.ReadFull(p.server, received)
								}
								if r.err != nil {
									break
								}
								if !bytes.Equal(payload, received) {
									r.err = fmt.Errorf("payload differs")
									break
								}
								if workload == "interactive" {
									if _, e := p.server.Write(received); e != nil {
										r.err = e
										break
									}
									if _, e := io.ReadFull(p.client, received); e != nil {
										r.err = e
										break
									}
									if !bytes.Equal(payload, received) {
										r.err = fmt.Errorf("reply differs")
										break
									}
									r.bytes += size
								}
								r.bytes += size
								r.latencies = append(r.latencies, time.Since(start))
							}
							results <- r
						}()
					}
					wg.Wait()
					elapsed := time.Since(started)
					close(results)
					close(stopSample)
					<-sampleDone
					var after runtime.MemStats
					runtime.ReadMemStats(&after)
					peak = max(peak, after.HeapAlloc)
					var latencies []time.Duration
					totalBytes := 0
					for r := range results {
						if r.err != nil {
							t.Fatal(r.err)
						}
						latencies = append(latencies, r.latencies...)
						totalBytes += r.bytes
					}
					sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
					var p95 time.Duration
					if len(latencies) > 0 {
						p95 = latencies[(95*len(latencies)+99)/100-1]
					}
					t.Logf("workload=%s driver=%s connections=%d seconds=%.6f app_bytes=%d MiBps=%.4f p95_us=%d alloc_bytes=%d allocations=%d peak_heap_bytes=%d peak_heap_delta_bytes=%d SDK_attempts=%d transport_sent=%d transport_received=%d", workload, network, concurrency, elapsed.Seconds(), totalBytes, float64(totalBytes)/(1<<20)/elapsed.Seconds(), p95.Microseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, peak, peak-before.HeapAlloc, requestTotal()-requestsBefore, metrics.GetBytesSent()-sentBefore, metrics.GetBytesReceived()-receivedBefore)
				})
			}
		}
	}
}

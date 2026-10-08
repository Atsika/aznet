package performance_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/atsika/aznet"
	"github.com/google/uuid"
)

// TestSDKWorkloadMeasurement is opt-in because it samples process memory and
// wall-clock latency against Azurite or explicitly selected live Azure accounts.
// Numbers describe this workload/environment, not general capacity or billing.
func TestSDKWorkloadMeasurement(t *testing.T) {
	if os.Getenv("AZNET_MEASURE") != "1" {
		t.Skip("set AZNET_MEASURE=1 with Azurite or an explicitly configured live account")
	}
	writeSize := measureInt(t, "AZNET_MEASURE_WRITE_SIZE", 64<<10)
	if writeSize < 8 {
		t.Fatal("write size must fit the 8-byte block number used to verify ordering")
	}
	streamBytes := measureInt(t, "AZNET_MEASURE_STREAM_BYTES", 4<<20)
	readSize := measureInt(t, "AZNET_MEASURE_READ_SIZE", writeSize)
	if streamBytes%writeSize != 0 {
		t.Fatal("stream bytes must be a multiple of write size")
	}
	idleTime := time.Duration(measureInt(t, "AZNET_MEASURE_IDLE_MS", 100)) * time.Millisecond
	for service, network := range []string{"azblob", "azqueue", "aztable"} {
		for _, concurrency := range []int{1, 4, 16} {
			for _, workload := range []string{"idle", "interactive", "bulk", "slow", "stream", "duplex", "wake"} {
				t.Run(fmt.Sprintf("%s/connections%d/%s", network, concurrency, workload), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()
					metrics := aznet.NewDefaultMetrics()
					setupStarted := time.Now()
					id := strings.ReplaceAll(uuid.NewString(), "-", "")
					t.Logf("owned_bootstrap=h%s,t%s", id, id)
					opts := []aznet.Option{aznet.WithContext(ctx), aznet.WithMetrics(metrics), aznet.WithEndpoints("h"+id, "t"+id), aznet.WithPing(0), aznet.WithAcceptPoll(time.Millisecond), aznet.WithDataPoll(10 * time.Millisecond), aznet.WithFastPoll(time.Millisecond)}
					if limit := measureInt(t, "AZNET_MEASURE_WRITE_LIMIT", 0); limit > 0 {
						opts = append(opts, aznet.WithBufferLimits(aznet.BufferLimits{Write: limit}))
						t.Logf("write_limit=%d", limit)
					}
					u := measurementAddress(t, network, service)
					ln, err := aznet.Listen(network, u.String(), opts...)
					if err != nil {
						t.Fatal(measurementError(err))
					}
					l := ln.(*aznet.Listener)
					defer func() {
						cleanupStarted := time.Now()
						if err := l.Close(); err != nil {
							t.Error(measurementError(err))
						}
						cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
						defer stop()
						if err := l.CleanupBootstrap(cleanup); err != nil {
							t.Error(measurementError(err))
						}
						t.Logf("phase=lifecycle listener_cleanup_ms=%.3f SDK_attempts=%d", float64(time.Since(cleanupStarted).Microseconds())/1000, requestTotal(metrics.RequestCounts()))
						logRequests(t, "lifecycle", metrics.RequestCounts(), nil)
					}()
					address, err := l.ConnectionString()
					if err != nil {
						t.Fatal(measurementError(err))
					}
					type pair struct{ client, server net.Conn }
					pairs := make([]pair, 0, concurrency)
					for range concurrency {
						type dialResult struct {
							conn net.Conn
							err  error
						}
						dial := make(chan dialResult, 1)
						go func() { c, e := aznet.Dial(network, address, opts...); dial <- dialResult{c, e} }()
						server, e := l.Accept()
						result := <-dial
						if e != nil || result.err != nil {
							t.Fatalf("accept/dial: %s / %s", measurementError(e), measurementError(result.err))
						}
						pairs = append(pairs, pair{result.conn, server})
						t.Logf("owned_session=%s,%s", result.conn.LocalAddr().(aznet.ServiceAddr).Resource, result.conn.RemoteAddr().(aznet.ServiceAddr).Resource)
						defer result.conn.Close()
					}
					t.Logf("phase=setup connections=%d setup_ms=%.3f SDK_attempts=%d", concurrency, float64(time.Since(setupStarted).Microseconds())/1000, requestTotal(metrics.RequestCounts()))
					// Exclude session setup and teardown from workload counters and timing.
					requestsBefore := metrics.RequestCounts()
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
						latencies     []time.Duration
						readLatencies []time.Duration
						bytes         int
						err           error
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
							if workload == "stream" || workload == "duplex" {
								forward := make(chan streamResult, 1)
								go func() { forward <- measureStream(p.client, p.server, writeSize, streamBytes, readSize, 17) }()
								if workload == "duplex" {
									reverse := measureStream(p.server, p.client, writeSize, streamBytes, readSize, 91)
									r.err = reverse.err
									r.bytes += reverse.bytes
									r.latencies = append(r.latencies, reverse.latencies...)
									r.readLatencies = append(r.readLatencies, reverse.readLatencies...)
								}
								f := <-forward
								r.err = errors.Join(r.err, f.err)
								r.bytes += f.bytes
								r.latencies = append(r.latencies, f.latencies...)
								r.readLatencies = append(r.readLatencies, f.readLatencies...)
								results <- r
								return
							}
							if workload == "idle" {
								p.server.SetReadDeadline(time.Now().Add(idleTime))
								var b [1]byte
								_, e := p.server.Read(b[:])
								p.server.SetReadDeadline(time.Time{})
								if !errors.Is(e, os.ErrDeadlineExceeded) {
									r.err = fmt.Errorf("idle read: %w", e)
								}
								results <- r
								return
							}
							if workload == "wake" {
								// Keep a read outstanding while the receiver backs off.
								got := make(chan error, 1)
								go func() {
									var b [1]byte
									_, err := io.ReadFull(p.server, b[:])
									if err == nil && b[0] != 42 {
										err = errors.New("wake payload differs")
									}
									got <- err
								}()
								time.Sleep(idleTime)
								start := time.Now()
								_, r.err = p.client.Write([]byte{42})
								r.err = errors.Join(r.err, <-got)
								r.latencies = []time.Duration{time.Since(start)}
								r.bytes = 1
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
					var readLatencies []time.Duration
					totalBytes := 0
					for r := range results {
						if r.err != nil {
							t.Fatal(measurementError(r.err))
						}
						latencies = append(latencies, r.latencies...)
						readLatencies = append(readLatencies, r.readLatencies...)
						totalBytes += r.bytes
					}
					sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
					sort.Slice(readLatencies, func(i, j int) bool { return readLatencies[i] < readLatencies[j] })
					var p95 time.Duration
					if len(latencies) > 0 {
						p95 = latencies[(95*len(latencies)+99)/100-1]
					}
					requestsAfter := metrics.RequestCounts()
					attempts := requestTotal(requestsAfter) - requestTotal(requestsBefore)
					var perMiB, perHour float64
					if totalBytes > 0 {
						perMiB = float64(attempts) * (1 << 20) / float64(totalBytes)
					}
					if workload == "idle" {
						perHour = float64(attempts) / elapsed.Hours() / float64(concurrency)
					}
					latencyKind := "delivery"
					applicationWriteSize := 64
					switch workload {
					case "idle":
						applicationWriteSize = 0
					case "wake":
						applicationWriteSize = 1
					case "bulk":
						applicationWriteSize = 256 << 10
					case "slow":
						applicationWriteSize = 32 << 10
					case "stream", "duplex":
						applicationWriteSize = writeSize
					}
					if workload == "interactive" {
						latencyKind = "roundtrip"
					}
					if workload == "stream" || workload == "duplex" {
						latencyKind = "write"
					}
					t.Logf("workload=%s driver=%s connections=%d seconds=%.6f app_bytes=%d MiBps=%.4f latency_kind=%s samples=%d p50_us=%d p95_us=%d p99_us=%d alloc_bytes=%d allocations=%d peak_heap_bytes=%d peak_heap_delta_bytes=%d SDK_attempts=%d attempts_per_MiB=%.3f idle_attempts_per_session_hour=%.1f transport_sent=%d transport_received=%d write_size=%d", workload, network, concurrency, elapsed.Seconds(), totalBytes, float64(totalBytes)/(1<<20)/elapsed.Seconds(), latencyKind, len(latencies), percentile(latencies, 50).Microseconds(), p95.Microseconds(), percentile(latencies, 99).Microseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, peak, peak-before.HeapAlloc, attempts, perMiB, perHour, metrics.GetBytesSent()-sentBefore, metrics.GetBytesReceived()-receivedBefore, applicationWriteSize)
					logRequests(t, "workload", requestsAfter, requestsBefore)
					if len(readLatencies) > 0 {
						t.Logf("read_size=%d read_samples=%d read_p50_ns=%d read_p95_ns=%d read_p99_ns=%d", readSize, len(readLatencies), percentile(readLatencies, 50).Nanoseconds(), percentile(readLatencies, 95).Nanoseconds(), percentile(readLatencies, 99).Nanoseconds())
					}
				})
			}
		}
	}
}

func measureInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			t.Fatalf("%s must be a positive integer", name)
		}
		return n
	}
	return fallback
}

func measurementAddress(t *testing.T, network string, service int) *url.URL {
	t.Helper()
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	path := os.Getenv("AZNET_MEASURE_LIVE_CONFIG")
	if path == "" {
		return &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", 10000+service), Path: "/devstoreaccount1", User: url.UserPassword("devstoreaccount1", key)}
	}
	account := os.Getenv("AZNET_MEASURE_" + strings.ToUpper(network) + "_ACCOUNT")
	if account == "" {
		t.Fatal("live measurement requires an explicit account for " + network)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read live measurement configuration")
	}
	var config struct {
		Listeners []struct {
			Driver  string `json:"driver"`
			Address string `json:"address"`
			Account string `json:"storage_account"`
			Key     string `json:"storage_account_key"`
		} `json:"listeners"`
	}
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid live measurement configuration")
	}
	for _, entry := range config.Listeners {
		if entry.Driver != network || entry.Account != account || entry.Key == "" {
			continue
		}
		u, err := url.Parse(entry.Address)
		if err != nil || u.Scheme != "https" || u.Host != account+"."+strings.TrimPrefix(network, "az")+".core.windows.net" {
			continue
		}
		t.Logf("environment=azure account=%s driver=%s", account, network)
		return &url.URL{Scheme: "https", Host: u.Host, User: url.UserPassword(account, entry.Key)}
	}
	t.Fatal("no matching live account-key configuration for " + network)
	return nil
}

// SDK error strings can contain SAS URLs. Only emit type/status/code, including
// for joined cleanup errors, so opt-in cloud runs never log credentials.
func measurementError(err error) string {
	if err == nil {
		return "nil"
	}
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Sprintf("status=%d code=%s", response.StatusCode, response.ErrorCode)
	}
	var network net.Error
	timeout := errors.As(err, &network) && network.Timeout()
	return fmt.Sprintf("error type %T deadline=%t canceled=%t timeout=%t (details omitted)",
		err, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded),
		errors.Is(err, context.Canceled), timeout)
}

func requestTotal(counts map[aznet.RequestAttempt]int64) int64 {
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}

func logRequests(t *testing.T, phase string, after, before map[aznet.RequestAttempt]int64) {
	t.Helper()
	var lines []string
	for a, n := range after {
		if n -= before[a]; n > 0 {
			lines = append(lines, fmt.Sprintf("phase=%s operation=%s method=%s status=%d retry=%t failed=%t attempts=%d", phase, a.Operation, a.Method, a.StatusCode, a.Retry, a.Failed, n))
		}
	}
	sort.Strings(lines)
	for _, line := range lines {
		t.Log(line)
	}
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(p*len(sorted)+99)/100-1]
}

type streamResult struct {
	latencies     []time.Duration
	readLatencies []time.Duration
	bytes         int
	err           error
}

// measureStream overlaps sending and receiving, verifies every byte (including
// an increasing block number), and waits for ordered EOF. Each useful byte is
// counted once, at the receiver. Latencies describe synchronous Write calls,
// not delivery; elapsed throughput includes receiver completion and FIN.
func measureStream(sender, receiver net.Conn, size, total, readSize int, seed byte) streamResult {
	writes := make(chan streamResult, 1)
	go func() {
		r := streamResult{}
		payload := bytes.Repeat([]byte{seed}, size)
		for i := range total / size {
			if size >= 8 {
				binary.LittleEndian.PutUint64(payload, uint64(i))
			}
			start := time.Now()
			n, err := sender.Write(payload)
			if err == nil && n != size {
				err = io.ErrShortWrite
			}
			if err != nil {
				r.err = err
				break
			}
			r.latencies = append(r.latencies, time.Since(start))
		}
		if r.err == nil {
			r.err = sender.(interface{ CloseWrite() error }).CloseWrite()
		}
		if r.err != nil {
			_ = receiver.SetReadDeadline(time.Now())
		}
		writes <- r
	}()
	r := streamResult{}
	reader := &measuredReader{reader: receiver, size: readSize, every: max(1, total/min(size, readSize)/8192), samples: make([]time.Duration, 0, 8192)}
	received := make([]byte, size)
	expected := bytes.Repeat([]byte{seed}, size)
	for i := range total / size {
		if size >= 8 {
			binary.LittleEndian.PutUint64(expected, uint64(i))
		}
		if _, r.err = io.ReadFull(reader, received); r.err != nil {
			break
		}
		if !bytes.Equal(received, expected) {
			r.err = errors.New("stream payload/order differs")
			break
		}
		r.bytes += size
	}
	if r.err == nil {
		var extra [1]byte
		n, err := receiver.Read(extra[:])
		if n != 0 || err != io.EOF {
			r.err = fmt.Errorf("ordered EOF: n=%d err=%v", n, err)
		}
	}
	if r.err != nil {
		_ = sender.SetWriteDeadline(time.Now())
	}
	w := <-writes
	r.err = errors.Join(r.err, w.err)
	r.latencies = w.latencies
	r.readLatencies = reader.samples
	return r
}

// Bound timing storage per direction. Systematic samples include both buffered
// and fetching Reads; they are application-call durations, not network RTT.
type measuredReader struct {
	reader             io.Reader
	size, every, calls int
	samples            []time.Duration
}

func (r *measuredReader) Read(p []byte) (int, error) {
	sample := r.calls%r.every == 0 && len(r.samples) < cap(r.samples)
	r.calls++
	if !sample {
		return r.reader.Read(p[:min(len(p), r.size)])
	}
	start := time.Now()
	n, err := r.reader.Read(p[:min(len(p), r.size)])
	r.samples = append(r.samples, time.Since(start))
	return n, err
}

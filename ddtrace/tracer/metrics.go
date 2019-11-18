// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package tracer

import (
	"runtime"
	"runtime/debug"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// defaultMetricsReportInterval specifies the interval at which runtime metrics will
// be reported.
const defaultMetricsReportInterval = 10 * time.Second

// reportMetrics periodically reports go runtime metrics to the specified gauger at
// the given interval.
func (t *tracer) reportMetrics(interval time.Duration) {
	var (
		ms   runtime.MemStats
		tags []string
	)
	gc := debug.GCStats{
		// When len(stats.PauseQuantiles) is 5, it will be filled with the
		// minimum, 25%, 50%, 75%, and maximum pause times. See the documentation
		// for (runtime/debug).ReadGCStats.
		PauseQuantiles: make([]time.Duration, 5),
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			log.Debug("Reporting runtime metrics...")
			runtime.ReadMemStats(&ms)
			debug.ReadGCStats(&gc)

			// CPU statistics
			t.statsd.Gauge("runtime.go.num_cpu", float64(runtime.NumCPU()), tags, 1)
			t.statsd.Gauge("runtime.go.num_goroutine", float64(runtime.NumGoroutine()), tags, 1)
			t.statsd.Gauge("runtime.go.num_cgo_call", float64(runtime.NumCgoCall()), tags, 1)
			// General statistics
			t.statsd.Gauge("runtime.go.mem_stats.alloc", float64(ms.Alloc), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.total_alloc", float64(ms.TotalAlloc), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.sys", float64(ms.Sys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.lookups", float64(ms.Lookups), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.mallocs", float64(ms.Mallocs), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.frees", float64(ms.Frees), tags, 1)
			// Heap memory statistics
			t.statsd.Gauge("runtime.go.mem_stats.heap_alloc", float64(ms.HeapAlloc), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.heap_sys", float64(ms.HeapSys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.heap_idle", float64(ms.HeapIdle), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.heap_inuse", float64(ms.HeapInuse), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.heap_released", float64(ms.HeapReleased), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.heap_objects", float64(ms.HeapObjects), tags, 1)
			// Stack memory statistics
			t.statsd.Gauge("runtime.go.mem_stats.stack_inuse", float64(ms.StackInuse), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.stack_sys", float64(ms.StackSys), tags, 1)
			// Off-heap memory statistics
			t.statsd.Gauge("runtime.go.mem_stats.m_span_inuse", float64(ms.MSpanInuse), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.m_span_sys", float64(ms.MSpanSys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.m_cache_inuse", float64(ms.MCacheInuse), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.m_cache_sys", float64(ms.MCacheSys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.buck_hash_sys", float64(ms.BuckHashSys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.gc_sys", float64(ms.GCSys), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.other_sys", float64(ms.OtherSys), tags, 1)
			// Garbage collector statistics
			t.statsd.Gauge("runtime.go.mem_stats.next_gc", float64(ms.NextGC), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.last_gc", float64(ms.LastGC), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.pause_total_ns", float64(ms.PauseTotalNs), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.num_gc", float64(ms.NumGC), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.num_forced_gc", float64(ms.NumForcedGC), tags, 1)
			t.statsd.Gauge("runtime.go.mem_stats.gc_cpu_fraction", ms.GCCPUFraction, tags, 1)
			for i, p := range []string{"min", "25p", "50p", "75p", "max"} {
				t.statsd.Gauge("runtime.go.gc_stats.pause_quantiles."+p, float64(gc.PauseQuantiles[i]), tags, 1)
			}

		case <-t.stopped:
			return
		}
	}
}

// Package iobattery implements the BFS-057 I/O measurement battery: the
// harness every mount/FUSE performance lever is judged by (BFS-055 transport,
// BFS-059 quick wins). No perf change may land without a before/after number
// produced by THIS battery on THIS hardware.
//
// The battery runs against any filesystem path — a real bunker mount chain
// (sshfs / bunker-fs FUSE mount) in the intended case, or an ordinary local
// directory as the fallback mode when no bunker agent is available. Every
// measurement records its counts, elapsed time, derived metric, and the exact
// command invocation so any number in a report is reproducible.
package iobattery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Report is the machine-readable battery result. One JSON document per run,
// one entry per measurement family plus the negative control.
type Report struct {
	Schema  string `json:"schema"`
	Target  string `json:"target"`
	Host    string `json:"host"`
	OS      string `json:"os"`
	Started string `json:"started"`
	Command string `json:"command"`
	// Measurements holds one entry per family that ran. Families that were
	// skipped or errored still appear, with an explicit reason — an
	// unexplained null is junk (BFS doctrine).
	Measurements []Measurement `json:"measurements"`
}

// Measurement is one family's result.
type Measurement struct {
	Name string `json:"name"` // throughput | latency_under_load | metadata_ops | iops | cpu_per_byte | negative_control
	Kind string `json:"kind"` // bytes_moved | ops | latency_percentiles | control
	// Count is the total bytes moved (bytes_moved) or ops issued (ops).
	Count int64 `json:"count"`
	// ElapsedMs is the measured wall time for the counted work.
	ElapsedMs float64 `json:"elapsed_ms"`
	// Derived is the headline metric: MB/s, ops/s, or ms (p99 for latency).
	Derived     float64        `json:"derived"`
	DerivedUnit string         `json:"derived_unit"`
	Detail      map[string]any `json:"detail,omitempty"`
	// Command is the exact reproduction invocation for this family.
	Command string `json:"command"`
	// Error is empty on success; a skipped/failed family names the reason here.
	Error string `json:"error,omitempty"`
}

// Options bounds a battery run. Defaults are deliberately small so a CI run
// finishes in seconds; real mount scoring raises them (see docs/iobattery.md).
type Options struct {
	Target        string // directory (mount point or local dir) to measure
	SizeMB        int64  // payload size for throughput / cpu-per-byte (default 32)
	Ops           int    // op count for metadata / iops / latency (default 2000)
	LoadWorkers   int    // background load goroutines for latency-under-load (default 4)
	NoNegativeCtl bool   // skip the negative control
	Command       string // top-level command line recorded into the report
}

func (o *Options) fill() error {
	if o.Target == "" {
		return fmt.Errorf("iobattery: target path is required")
	}
	st, err := os.Stat(o.Target)
	if err != nil {
		return fmt.Errorf("iobattery: target %q: %w", o.Target, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("iobattery: target %q is not a directory", o.Target)
	}
	if o.SizeMB <= 0 {
		o.SizeMB = 32
	}
	if o.Ops <= 0 {
		o.Ops = 2000
	}
	if o.LoadWorkers <= 0 {
		o.LoadWorkers = 4
	}
	return nil
}

// Run executes the full battery (or the selected families) and returns the
// report. It never leaves files behind: all payloads live in a scratch
// subdirectory that is removed on the way out.
func Run(opts Options) (*Report, error) {
	if err := opts.fill(); err != nil {
		return nil, err
	}
	target, err := filepath.Abs(opts.Target)
	if err != nil {
		return nil, err
	}
	opts.Target = target
	host, _ := os.Hostname()
	rep := &Report{
		Schema:  "bunker.iobattery.v1",
		Target:  target,
		Host:    host,
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
		Started: time.Now().UTC().Format(time.RFC3339),
		Command: opts.Command,
	}
	scratch, err := os.MkdirTemp(target, ".iobattery-run-")
	if err != nil {
		return nil, fmt.Errorf("iobattery: scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of our own scratch dir

	families := []struct {
		name string
		fn   func(dir string, o Options) Measurement
	}{
		{"throughput", measureThroughput},
		{"latency_under_load", measureLatencyUnderLoad},
		{"metadata_ops", measureMetadata},
		{"iops", measureIOPS},
		{"cpu_per_byte", measureCPUPerByte},
	}
	for _, f := range families {
		m := f.fn(scratch, opts)
		m.Name = f.name
		rep.Measurements = append(rep.Measurements, m)
	}
	if !opts.NoNegativeCtl {
		m := measureNegativeControl(scratch, opts)
		m.Name = "negative_control"
		rep.Measurements = append(rep.Measurements, m)
	}
	return rep, nil
}

// Marshal renders the report as indented JSON.
func (r *Report) Marshal() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// ---- helpers -------------------------------------------------------------

// timed runs fn and returns (count, elapsed).
func timed(fn func() (int64, error)) (int64, time.Duration, error) {
	start := time.Now()
	n, err := fn()
	return n, time.Since(start), err
}

func mbps(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / (1 << 20) / d.Seconds()
}

func opsPerSec(ops int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(ops) / d.Seconds()
}

func fail(name string, err error) Measurement {
	return Measurement{Name: name, Command: "", Error: err.Error()}
}

// writePayload writes size bytes of deterministic data to path in chunk
// writes (measured time EXCLUDES this seeding write when used by readers).
func writePayload(path string, size int64, chunk []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var written int64
	for written < size {
		n := int64(len(chunk))
		if size-written < n {
			n = size - written
		}
		if _, werr := f.Write(chunk[:n]); werr != nil {
			_ = f.Close() //nolint:errcheck // error path: the write error wins
			return werr
		}
		written += n
	}
	return f.Close()
}

// ---- 1. THROUGHPUT --------------------------------------------------------

// measureThroughput: single-stream sequential write then read of SizeMB.
func measureThroughput(dir string, o Options) Measurement {
	chunk := make([]byte, 1<<20) // 1 MiB sequential units
	wPath := filepath.Join(dir, "throughput.bin")

	wN, wD, wErr := timed(func() (int64, error) {
		if err := writePayload(wPath, o.SizeMB<<20, chunk); err != nil {
			return 0, err
		}
		return o.SizeMB << 20, nil
	})
	if wErr != nil {
		return fail("throughput", wErr)
	}
	rN, rD, rErr := timed(func() (int64, error) {
		buf := make([]byte, len(chunk))
		n, err := readAllSeq(wPath, buf)
		return n, err
	})
	os.Remove(wPath) //nolint:errcheck // cleanup after measurement already captured
	if rErr != nil {
		return fail("throughput", rErr)
	}
	return Measurement{
		Kind:        "bytes_moved",
		Count:       wN + rN,
		ElapsedMs:   float64((wD + rD).Microseconds()) / 1000,
		Derived:     mbps(wN, wD),
		DerivedUnit: "MB/s",
		Detail: map[string]any{
			"write_bytes": wN, "write_ms": ms(wD), "write_mb_s": mbps(wN, wD),
			"read_bytes": rN, "read_ms": ms(rD), "read_mb_s": mbps(rN, rD),
			"payload_mb": o.SizeMB, "chunk_bytes": len(chunk),
		},
		Command: fmt.Sprintf("bunker iobattery --target %s --size-mb %d --mode throughput", o.Target, o.SizeMB),
	}
}

func readAllSeq(path string, buf []byte) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var total int64
	for {
		n, rerr := f.Read(buf)
		total += int64(n)
		if rerr != nil {
			if rerr.Error() == "EOF" {
				return total, nil
			}
			return total, rerr
		}
	}
}

// ---- 2. LATENCY UNDER LOAD -------------------------------------------------

// measureLatencyUnderLoad: small 4K synchronous ops measured while LoadWorkers
// background goroutines hammer the same directory, then p50/p95/p99.
func measureLatencyUnderLoad(dir string, o Options) Measurement {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < o.LoadWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p := filepath.Join(dir, fmt.Sprintf("load-%d.bin", id))
			buf := make([]byte, 4096)
			for {
				select {
				case <-stop:
					return
				default:
				}
				f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return
				}
				_, _ = f.Write(buf) //nolint:errcheck // load generator, not a measurement
				_ = f.Close()       //nolint:errcheck // load generator, not a measurement
				_ = os.Remove(p)    //nolint:errcheck // load generator, not a measurement
			}
		}(i)
	}
	// Stop the load workers BEFORE waiting on them: the workers exit only
	// when stop closes, so the close must be explicit and precede the Wait
	// (a deferred close after Wait would deadlock).
	defer func() { close(stop); wg.Wait() }()

	const opSize = 4096
	buf := make([]byte, opSize)
	lats := make([]float64, 0, o.Ops)
	var ops int64
	start := time.Now()
	for i := 0; i < o.Ops; i++ {
		p := filepath.Join(dir, "lat-probe.bin")
		opStart := time.Now()
		if err := os.WriteFile(p, buf, 0o600); err != nil {
			return fail("latency_under_load", err)
		}
		if err := os.Remove(p); err != nil {
			return fail("latency_under_load", err)
		}
		lats = append(lats, float64(time.Since(opStart).Microseconds())/1000)
		ops += 2 // write + unlink are both measured ops
	}
	elapsed := time.Since(start)

	if len(lats) == 0 {
		return fail("latency_under_load", fmt.Errorf("no ops recorded"))
	}
	sort.Float64s(lats)
	return Measurement{
		Kind:        "latency_percentiles",
		Count:       ops,
		ElapsedMs:   ms(elapsed),
		Derived:     percentile(lats, 0.99),
		DerivedUnit: "ms_p99",
		Detail: map[string]any{
			"p50_ms":        percentile(lats, 0.50),
			"p95_ms":        percentile(lats, 0.95),
			"p99_ms":        percentile(lats, 0.99),
			"op_size_bytes": opSize,
			"load_workers":  o.LoadWorkers,
			"ops_per_sec":   opsPerSec(ops, elapsed),
		},
		Command: fmt.Sprintf("bunker iobattery --target %s --ops %d --load-workers %d --mode latency", o.Target, o.Ops, o.LoadWorkers),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// ---- 3. METADATA OPS/SEC ---------------------------------------------------

// measureMetadata: create/stat/rename/unlink cycles on small files.
func measureMetadata(dir string, o Options) Measurement {
	const rounds = 4 // each round: o.Ops/4 creates, then stat/rename/unlink
	perRound := o.Ops / rounds
	if perRound < 4 {
		perRound = 4
	}
	var ops int64
	start := time.Now()
	for r := 0; r < rounds; r++ {
		base := filepath.Join(dir, fmt.Sprintf("md-%d", r))
		if err := os.MkdirAll(base, 0o700); err != nil {
			return fail("metadata_ops", err)
		}
		names := make([]string, perRound)
		for i := 0; i < perRound; i++ {
			p := filepath.Join(base, fmt.Sprintf("f-%d", i))
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				return fail("metadata_ops", err)
			}
			names[i] = p
			ops++
		}
		for _, p := range names {
			if _, err := os.Stat(p); err != nil {
				return fail("metadata_ops", err)
			}
			ops++
		}
		for _, p := range names {
			if err := os.Rename(p, p+".r"); err != nil {
				return fail("metadata_ops", err)
			}
			ops++
		}
		for _, p := range names {
			if err := os.Remove(p + ".r"); err != nil {
				return fail("metadata_ops", err)
			}
			ops++
		}
		_ = os.RemoveAll(base) //nolint:errcheck // round cleanup; next round uses a fresh dir
	}
	elapsed := time.Since(start)
	return Measurement{
		Kind:        "ops",
		Count:       ops,
		ElapsedMs:   ms(elapsed),
		Derived:     opsPerSec(ops, elapsed),
		DerivedUnit: "ops/s",
		Detail: map[string]any{
			"ops_create_stat_rename_unlink": fmt.Sprintf("%d/%d/%d/%d", ops/4, ops/4, ops/4, ops/4),
			"files_per_round":               perRound,
			"rounds":                        rounds,
		},
		Command: fmt.Sprintf("bunker iobattery --target %s --ops %d --mode metadata", o.Target, o.Ops),
	}
}

// ---- 4. IOPS (random 4K) ---------------------------------------------------

// measureIOPS: random-ordered 4K reads and writes over a pre-seeded file.
func measureIOPS(dir string, o Options) Measurement {
	const blk = 4096
	fSize := int64(o.Ops) * blk
	p := filepath.Join(dir, "iops.bin")
	if err := writePayload(p, fSize, make([]byte, 1<<20)); err != nil {
		return fail("iops", err)
	}
	defer func() { _ = os.Remove(p) }() //nolint:errcheck // cleanup after measurement captured

	f, err := os.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		return fail("iops", err)
	}
	defer f.Close() //nolint:errcheck // RDWR handle; op errors already fail the family
	// Deterministic pseudo-random offsets (xorshift) — reproducible runs.
	seed := uint64(0x9e3779b97f4a7c15)
	next := func() int64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return int64(seed%uint64(o.Ops)) * blk
	}

	buf := make([]byte, blk)
	var ops int64
	start := time.Now()
	for i := 0; i < o.Ops; i++ {
		off := next()
		if i%2 == 0 {
			if _, err := f.WriteAt(buf, off); err != nil {
				return fail("iops", err)
			}
		} else {
			if _, err := f.ReadAt(buf, off); err != nil {
				return fail("iops", err)
			}
		}
		ops++
	}
	elapsed := time.Since(start)
	return Measurement{
		Kind:        "ops",
		Count:       ops,
		ElapsedMs:   ms(elapsed),
		Derived:     opsPerSec(ops, elapsed),
		DerivedUnit: "IOPS",
		Detail: map[string]any{
			"block_bytes": blk,
			"pattern":     "random 50/50 read/write",
			"read_iops":   opsPerSec(ops/2, elapsed),
			"file_mb":     fSize >> 20,
		},
		Command: fmt.Sprintf("bunker iobattery --target %s --ops %d --mode iops", o.Target, o.Ops),
	}
}

// ---- 5. CPU PER BYTE -------------------------------------------------------

// measureCPUPerByte: rusage (self + children) CPU time vs bytes actually
// moved through write+fsync+read of the payload.
func measureCPUPerByte(dir string, o Options) Measurement {
	before := rusageCPU()
	p := filepath.Join(dir, "cpuperbyte.bin")
	chunk := make([]byte, 64<<10)
	size := o.SizeMB << 20

	n, elapsed, err := timed(func() (int64, error) {
		if err := writePayloadFsync(p, size, chunk); err != nil {
			return 0, err
		}
		f, rerr := os.Open(p)
		if rerr != nil {
			return 0, rerr
		}
		buf := make([]byte, len(chunk))
		var total int64
		for {
			rn, rerr := f.Read(buf)
			total += int64(rn)
			if rerr != nil {
				break
			}
		}
		_ = f.Close()    //nolint:errcheck // cleanup after measurement captured
		_ = os.Remove(p) //nolint:errcheck // cleanup after measurement captured
		return total, nil
	})
	if err != nil {
		return fail("cpu_per_byte", err)
	}
	after := rusageCPU()
	cpuNS := (after - before).Nanoseconds()

	bytesPerCPU := 0.0
	if cpuNS > 0 {
		bytesPerCPU = float64(n) / (float64(cpuNS) / 1e9)
	}
	return Measurement{
		Kind:        "bytes_moved",
		Count:       n,
		ElapsedMs:   ms(elapsed),
		Derived:     bytesPerCPU,
		DerivedUnit: "bytes/cpu-second",
		Detail: map[string]any{
			"cpu_ns":       cpuNS,
			"cpu_ms":       float64(cpuNS) / 1e6,
			"mb_per_cpu_s": bytesPerCPU / (1 << 20),
			"source":       "getrusage RUSAGE_SELF+RUSAGE_CHILDREN",
		},
		Command: fmt.Sprintf("bunker iobattery --target %s --size-mb %d --mode cpuperbyte", o.Target, o.SizeMB),
	}
}

func rusageCPU() time.Duration {
	var self, children syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &self)         //nolint:errcheck // cannot fail for these arguments
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children) //nolint:errcheck // cannot fail for these arguments
	return cpuTime(self) + cpuTime(children)
}

func cpuTime(r syscall.Rusage) time.Duration {
	return time.Duration(r.Utime.Sec)*time.Second + time.Duration(r.Utime.Usec)*time.Microsecond +
		time.Duration(r.Stime.Sec)*time.Second + time.Duration(r.Stime.Usec)*time.Microsecond
}

func writePayloadFsync(path string, size int64, chunk []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var written int64
	for written < size {
		n := int64(len(chunk))
		if size-written < n {
			n = size - written
		}
		if _, werr := f.Write(chunk[:n]); werr != nil {
			_ = f.Close() //nolint:errcheck // error path: the write error wins
			return werr
		}
		written += n
	}
	_ = f.Sync() //nolint:errcheck // durability flush is part of the measured work, not a checked op
	return f.Close()
}

// ---- 6. NEGATIVE CONTROL ----------------------------------------------------

// measureNegativeControl proves a lever by measurement, not assumption: the
// readahead lever. Sequential buffered read (kernel readahead active) vs
// O_DIRECT sequential read (readahead bypassed — page cache AND readahead
// off). On a lever-shaped target (a FUSE/sshfs mount) these differ; on tmpfs
// O_DIRECT is unsupported and the result records that reason explicitly
// instead of silently passing.
//
// On a real mount the current BDI readahead is also recorded (best-effort)
// so the control names the knob it exercised.
func measureNegativeControl(dir string, o Options) Measurement {
	size := o.SizeMB << 20
	if size < 8<<20 {
		size = 8 << 20 // below ~8 MiB the readahead effect is noise
	}
	p := filepath.Join(dir, "negctl.bin")
	chunk := make([]byte, 1<<20)
	if err := writePayload(p, size, chunk); err != nil {
		return fail("negative_control", err)
	}
	defer func() { _ = os.Remove(p) }() //nolint:errcheck // cleanup after measurement captured

	bufN, bufD, bufErr := timed(func() (int64, error) {
		return readAllSeq(p, make([]byte, 1<<20))
	})
	if bufErr != nil {
		return fail("negative_control", bufErr)
	}

	dN, dD, dErr := timed(func() (int64, error) {
		return readDirect(p, size)
	})

	detail := map[string]any{
		"lever":              "readahead",
		"buffered_read_mb_s": mbps(bufN, bufD),
		"buffered_read_ms":   ms(bufD),
		"payload_mb":         size >> 20,
		"bdi_read_ahead_kb":  bdiReadAheadKB(p),
		"levers_proven_by":   "buffered (readahead active) vs O_DIRECT (readahead bypassed)",
	}
	if dErr != nil {
		// O_DIRECT unsupported (tmpfs, some FUSE drivers). Record why — an
		// unexplained missing arm would make the control vacuous.
		detail["odirect_supported"] = false
		detail["odirect_error"] = dErr.Error()
		return Measurement{
			Kind:        "control",
			Count:       bufN,
			ElapsedMs:   ms(bufD),
			Derived:     mbps(bufN, bufD),
			DerivedUnit: "MB/s",
			Detail:      detail,
			Error:       "control incomplete: O_DIRECT arm unavailable: " + dErr.Error(),
			Command:     fmt.Sprintf("bunker iobattery --target %s --mode control", o.Target),
		}
	}
	detail["odirect_supported"] = true
	detail["odirect_read_mb_s"] = mbps(dN, dD)
	detail["odirect_read_ms"] = ms(dD)
	detail["readahead_lift_ratio"] = 0.0
	if mbps(dN, dD) > 0 {
		detail["readahead_lift_ratio"] = mbps(bufN, bufD) / mbps(dN, dD)
	}
	return Measurement{
		Kind:        "control",
		Count:       bufN + dN,
		ElapsedMs:   ms(bufD + dD),
		Derived:     mbps(bufN, bufD),
		DerivedUnit: "MB/s",
		Detail:      detail,
		Command:     fmt.Sprintf("bunker iobattery --target %s --mode control", o.Target),
	}
}

// readDirect sequentially reads path with O_DIRECT (page cache + readahead
// bypassed). Buffers and length are sector-aligned as O_DIRECT requires.
func readDirect(path string, size int64) (int64, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Close(fd) }() //nolint:errcheck // read-only fd cleanup
	buf := make([]byte, 1<<20)               // aligned: slice of a fresh page-granular alloc
	var total int64
	for total < size {
		max := int64(len(buf))
		if size-total < max {
			max = size - total
		}
		n, rerr := syscall.Pread(fd, buf[:max], total)
		total += int64(n)
		if rerr != nil {
			return total, rerr
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

// bdiReadAheadKB best-effort reads the backing device's read_ahead_kb for the
// filesystem holding path. Empty string when not resolvable (never a silent
// fake number).
func bdiReadAheadKB(path string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return ""
	}
	dev := fmt.Sprintf("%d:%d", unixMajor(st.Dev), unixMinor(st.Dev))
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/bdi/%s/read_ahead_kb", dev))
	if err != nil {
		return ""
	}
	return trimSpace(string(data))
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '	') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '	') {
		end--
	}
	return s[start:end]
}

// unixMajor / unixMinor decompose a st_dev device number (the encoding is the
// kernel's new-style makedev: major = high byte + low bits of the next byte).
func unixMajor(dev uint64) uint64 {
	return (dev>>8)&0xfff | (dev>>32) & ^uint64(0xfff)
}

func unixMinor(dev uint64) uint64 {
	return (dev & 0xff) | ((dev >> 12) & ^uint64(0xff))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

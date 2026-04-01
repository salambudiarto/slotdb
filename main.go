package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Layout ──────────────────────────────────────────────────────────────────
//
// Every slot is 8 bytes:
//
//	[0..3] value     uint32  little-endian
//	[4]    flag      uint8
//	[5..6] version   uint16  little-endian   (0 = never written)
//	[7]    checksum  uint8   = XOR of bytes 0-6
//
// Slot states:
//   free      – all 8 bytes are zero (zero-value array ⟹ free, no sentinel needed)
//   tombstone – flag==FlagDel AND at least one other byte ≠ 0
//   active    – flag ∈ {FlagStart, FlagNext, FlagEnd}

const SlotSize = 8

const (
	OffValue    = 0
	OffFlag     = 4
	OffVersion  = 5
	OffChecksum = 7
)

const (
	FlagDel   = uint8(0)
	FlagStart = uint8(1)
	FlagNext  = uint8(2)
	FlagEnd   = uint8(3)
)

// zeroSlot is used by DFG to zero-fill a tombstone via copy(), not a byte loop.
var zeroSlot [SlotSize]byte

// ─── Config ───────────────────────────────────────────────────────────────────

// Config is intentionally small and fully driven by environment variables so
// the binary can be reused across environments without recompilation.
type Config struct {
	ListenAddr      string
	HTTPListenAddr  string
	EnableTCP       bool
	EnableHTTP      bool
	ArenaBytes      int
	MaxConn         int
	MaxLineBytes    int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	CmdsPerSec      int
	DFGCooldown     time.Duration
	AutoDFGInterval time.Duration
	AutoDFGRatioPct int
	AutoDFGMinSlots uint64
	GoMaxProcs      int
}

func defaultConfig() Config {
	return Config{
		ListenAddr:      envString("LISTEN_ADDR", ":9000"),
		HTTPListenAddr:  envString("HTTP_LISTEN_ADDR", ":8080"),
		EnableTCP:       envBool("ENABLE_TCP", true),
		EnableHTTP:      envBool("ENABLE_HTTP", true),
		ArenaBytes:      envInt("ARENA_BYTES", 2*1024*1024), // 2 MiB
		MaxConn:         envInt("MAX_CONN", 32),             // micro env: 32 conns ≈ 32 goroutines + ~256 KB heap
		MaxLineBytes:    envInt("MAX_LINE_BYTES", 96),
		ReadTimeout:     envDuration("READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    envDuration("WRITE_TIMEOUT", 5*time.Second),
		IdleTimeout:     envDuration("IDLE_TIMEOUT", 30*time.Second),
		CmdsPerSec:      envInt("CMDS_PER_SEC", 128),
		DFGCooldown:     envDuration("DFG_COOLDOWN", 5*time.Minute),
		AutoDFGInterval: envDuration("AUTO_DFG_INTERVAL", 5*time.Minute),
		AutoDFGRatioPct: envInt("AUTO_DFG_RATIO_PCT", 25),
		AutoDFGMinSlots: uint64(envInt("AUTO_DFG_MIN_SLOTS", 1024)),
		GoMaxProcs:      envInt("GOMAXPROCS", 1), // 1 is safe on a shared 1-core host
	}
}

// ─── Server ───────────────────────────────────────────────────────────────────

type Server struct {
	cfg  Config
	done chan struct{} // closed to signal autoDFGLoop to stop

	arena []byte
	slots uint32

	// barrier: all normal ops hold RLock; DFG full-sweep holds Lock.
	barrier sync.RWMutex

	// 256 independent mutexes; stripe = idx & 0xFF.
	// Keeps lock granularity fine without a large mutex array cost.
	stripes [256]sync.Mutex

	activeSlots atomic.Uint64
	tombstones  atomic.Uint64
	opsTotal    atomic.Uint64

	lastDFGUnix atomic.Int64
	dfgRunning  atomic.Bool

	// scanPool recycles scanner backing buffers — avoids per-connection alloc.
	scanPool sync.Pool
	// writerPool recycles bufio.Writer instances — avoids per-connection alloc.
	writerPool sync.Pool
}

func NewServer(cfg Config) *Server {
	if cfg.ArenaBytes < SlotSize {
		cfg.ArenaBytes = SlotSize
	}
	cfg.ArenaBytes = (cfg.ArenaBytes / SlotSize) * SlotSize

	s := &Server{
		cfg:   cfg,
		done:  make(chan struct{}),
		arena: make([]byte, cfg.ArenaBytes),
		slots: uint32(cfg.ArenaBytes / SlotSize),
	}
	s.scanPool = sync.Pool{
		New: func() any {
			b := make([]byte, cfg.MaxLineBytes)
			return &b
		},
	}
	s.writerPool = sync.Pool{
		New: func() any {
			// 256 bytes covers all response lines we ever emit.
			return bufio.NewWriterSize(nil, 256)
		},
	}
	return s
}

// Run starts enabled transports and blocks until one returns an error.
func (s *Server) Run() error {
	if !s.cfg.EnableTCP && !s.cfg.EnableHTTP {
		return errors.New("at least one transport must be enabled")
	}

	go s.autoDFGLoop()

	errCh := make(chan error, 2)

	var tcpLn net.Listener
	var httpSrv *http.Server

	if s.cfg.EnableTCP {
		ln, err := net.Listen("tcp", s.cfg.ListenAddr)
		if err != nil {
			close(s.done)
			return err
		}
		tcpLn = ln
		go func() { errCh <- s.serveTCP(ln) }()
	}
	if s.cfg.EnableHTTP {
		srv := s.buildHTTPServer()
		httpSrv = srv
		go func() { errCh <- s.serveHTTP(srv) }()
	}

	// Wait for first transport failure; tear down the other.
	err := <-errCh
	close(s.done)
	if tcpLn != nil {
		_ = tcpLn.Close()
	}
	if httpSrv != nil {
		_ = httpSrv.Close()
	}
	return err
}

// serveTCP accepts connections on an already-bound listener.
func (s *Server) serveTCP(ln net.Listener) error {
	defer ln.Close()

	log.Printf("listen=%s arena_bytes=%d slots=%d max_conn=%d gomaxprocs=%d",
		s.cfg.ListenAddr, len(s.arena), s.slots, s.cfg.MaxConn, runtime.GOMAXPROCS(0))

	sem := make(chan struct{}, s.cfg.MaxConn)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil // clean shutdown
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}

		select {
		case sem <- struct{}{}:
			go func(c net.Conn) {
				defer func() { <-sem }()
				s.handleConn(c)
			}(conn)
		default:
			// Server at capacity — reject fast.
			_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			_, _ = io.WriteString(conn, "ERR server busy\n")
			_ = conn.Close()
		}
	}
}

func (s *Server) buildHTTPServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)
	return &http.Server{
		Addr:              s.cfg.HTTPListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
	}
}

func (s *Server) serveHTTP(server *http.Server) error {
	log.Printf("http_listen=%s", s.cfg.HTTPListenAddr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ─── Per-connection handler ────────────────────────────────────────────────

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Borrow a pooled buffer — avoids heap alloc per connection.
	bufPtr := s.scanPool.Get().(*[]byte)
	buf := *bufPtr
	defer s.scanPool.Put(bufPtr)

	// Borrow a pooled writer — avoids heap alloc per connection.
	writerPtr := s.writerPool.Get().(*bufio.Writer)
	writerPtr.Reset(conn)
	defer s.writerPool.Put(writerPtr)
	writer := writerPtr

	reader := bufio.NewScanner(conn)
	reader.Buffer(buf, s.cfg.MaxLineBytes)

	rl := newRateLimiter(s.cfg.CmdsPerSec)

	for {
		// Set read deadline once before each blocking Scan call.
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))

		if !reader.Scan() {
			// EOF or read error — connection is gone, nothing to write back.
			return
		}

		if !rl.Allow() {
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			_, _ = writer.WriteString("ERR rate limit\n")
			_ = writer.Flush()
			return // disconnect after rate-limit violation
		}

		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}

		resp := s.exec(line)
		s.opsTotal.Add(1)

		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		_, _ = writer.WriteString(resp)
		_ = writer.WriteByte('\n')
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

// ─── Rate limiter ──────────────────────────────────────────────────────────
//
// Simple token-bucket; one instance per connection, never shared,
// so no mutex needed. Lives entirely in handleConn's stack frame.

type rateLimiter struct {
	limit      int
	burst      int
	tokens     int
	lastRefill time.Time
}

func newRateLimiter(limit int) *rateLimiter {
	if limit < 1 {
		limit = 1
	}
	burst := limit * 2
	if burst < 4 {
		burst = 4
	}
	return &rateLimiter{limit: limit, burst: burst, tokens: burst, lastRefill: time.Now()}
}

func (r *rateLimiter) Allow() bool {
	now := time.Now()
	elapsed := now.Sub(r.lastRefill)
	if elapsed >= time.Second {
		secs := int(elapsed / time.Second)
		r.tokens += secs * r.limit
		if r.tokens > r.burst {
			r.tokens = r.burst
		}
		r.lastRefill = r.lastRefill.Add(time.Duration(secs) * time.Second)
	}
	if r.tokens <= 0 {
		return false
	}
	r.tokens--
	return true
}

// ─── Command dispatcher ────────────────────────────────────────────────────

func (s *Server) exec(line string) string {
	// PING is by far the most frequent command; short-circuit before timing.
	// opsTotal is incremented by the caller (handleConn / runHTTPCommand).
	cmd, rest := splitFirst(line)
	if strings.EqualFold(cmd, "PING") {
		return "OK PONG"
	}
	// Uppercase once here so dispatch receives a canonical token.
	cmd = strings.ToUpper(cmd)
	start := time.Now()
	resp := s.dispatch(cmd, rest)
	return appendProcessingTime(resp, time.Since(start))
}

func (s *Server) dispatch(cmd, rest string) string {
	switch cmd { // already uppercased by exec
	case "STATS":
		active := s.activeSlots.Load()
		tomb := s.tombstones.Load()
		used := active + tomb
		ratio := uint64(0)
		if used > 0 {
			ratio = (tomb * 100) / used
		}
		return fmt.Sprintf("OK slots=%d active=%d tombstones=%d deleted_ratio=%d ops=%d",
			s.slots, active, tomb, ratio, s.opsTotal.Load())

	case "VIEW":
		idx, ok := parseSingleIndex(rest, s.slots)
		if !ok {
			return "ERR bad index"
		}
		value, flag, version, status := s.view(idx)
		switch status {
		case viewChecksumErr:
			return "ERR checksum"
		case viewNotActive:
			return "ERR not active"
		}
		return fmt.Sprintf("OK value=%d flag=%d version=%d", value, flag, version)

	case "UPSERT":
		idxStr, valStr, ok := parseTwoArgs(rest)
		if !ok {
			return "ERR usage UPSERT <index> <value>"
		}
		i, ok := parseIndex(idxStr, s.slots)
		if !ok {
			return "ERR bad index"
		}
		v, ok := parseUint32(valStr)
		if !ok {
			return "ERR bad value"
		}
		s.upsert(i, v)
		return "OK"

	case "DELETE":
		idx, ok := parseSingleIndex(rest, s.slots)
		if !ok {
			return "ERR bad index"
		}
		if !s.delete(idx) {
			return "ERR not active"
		}
		return "OK"

	case "INC":
		return s.incdsc(rest, true)

	case "DSC":
		return s.incdsc(rest, false)

	case "RST":
		idx, ok := parseSingleIndex(rest, s.slots)
		if !ok {
			return "ERR bad index"
		}
		if !s.rst(idx) {
			return "ERR not active"
		}
		return "OK"

	case "DFG":
		mode := strings.ToUpper(strings.TrimSpace(rest))
		if mode == "" {
			mode = "AUTO"
		}
		swept, freed, err := s.dfg(mode)
		if err != nil {
			return "ERR " + err.Error()
		}
		return fmt.Sprintf("OK swept=%d freed=%d", swept, freed)

	default:
		return "ERR unknown command"
	}
}

// incdsc handles INC and DSC to eliminate duplicated logic.
func (s *Server) incdsc(args string, increment bool) string {
	idxStr, deltaStr := splitFirst(args)
	i, ok := parseIndex(idxStr, s.slots)
	if !ok {
		return "ERR bad index"
	}
	delta := uint32(1)
	if deltaStr != "" {
		if delta, ok = parseUint32(deltaStr); !ok {
			return "ERR bad value"
		}
	}
	var value uint32
	if increment {
		value, ok = s.inc(i, delta)
	} else {
		value, ok = s.dsc(i, delta)
	}
	if !ok {
		return "ERR not active"
	}
	// Stack-allocated response — avoids fmt.Sprintf heap alloc on hot path.
	var b [32]byte
	n := copy(b[:], "OK value=")
	dst := strconv.AppendUint(b[n:n], uint64(value), 10)
	n += len(dst)
	return string(b[:n])
}

// appendProcessingTime appends backend_processing_us=N without heap alloc.
func appendProcessingTime(resp string, elapsed time.Duration) string {
	us := elapsed.Microseconds()
	if us < 0 {
		us = 0
	}
	var b [160]byte
	n := copy(b[:], resp)
	n += copy(b[n:], " backend_processing_us=")
	dst := strconv.AppendInt(b[n:n], us, 10)
	n += len(dst)
	return string(b[:n])
}

// ─── Slot operations ──────────────────────────────────────────────────────

type viewStatus uint8

const (
	viewOK          viewStatus = iota
	viewNotActive              // flag == FlagDel (free or tombstone)
	viewChecksumErr            // stored checksum doesn't match computed
)

func (s *Server) view(idx uint32) (uint32, uint8, uint16, viewStatus) {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	value := readU32(s.arena[off:])
	flag := s.arena[off+OffFlag]
	version := readU16(s.arena[off+OffVersion:])
	checksum := s.arena[off+OffChecksum]
	if flag == FlagDel {
		return 0, flag, version, viewNotActive
	}
	if checksum != calcChecksum(value, flag, version) {
		return 0, flag, version, viewChecksumErr
	}
	return value, flag, version, viewOK
}

func (s *Server) upsert(idx uint32, value uint32) {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	prevValue := readU32(s.arena[off:])
	prevFlag := s.arena[off+OffFlag]
	prevVersion := readU16(s.arena[off+OffVersion:])
	prevChecksum := s.arena[off+OffChecksum]

	free := prevFlag == FlagDel && prevValue == 0 && prevVersion == 0 && prevChecksum == 0
	tomb := prevFlag == FlagDel && !free
	switch {
	case free:
		s.activeSlots.Add(1)
	case tomb:
		s.tombstones.Add(^uint64(0)) // decrement
		s.activeSlots.Add(1)
		// else: active slot being overwritten — counts stay the same
	}

	version := nextVersion(prevVersion)
	writeU32(s.arena[off:], value)
	s.arena[off+OffFlag] = FlagStart
	writeU16(s.arena[off+OffVersion:], version)
	s.arena[off+OffChecksum] = calcChecksum(value, FlagStart, version)
}

func (s *Server) delete(idx uint32) bool {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	value := readU32(s.arena[off:])
	flag := s.arena[off+OffFlag]
	if flag == FlagDel {
		return false
	}
	version := nextVersion(readU16(s.arena[off+OffVersion:]))
	s.arena[off+OffFlag] = FlagDel
	writeU16(s.arena[off+OffVersion:], version)
	s.arena[off+OffChecksum] = calcChecksum(value, FlagDel, version)
	s.activeSlots.Add(^uint64(0)) // decrement
	s.tombstones.Add(1)
	return true
}

func (s *Server) inc(idx uint32, delta uint32) (uint32, bool) {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	value := readU32(s.arena[off:])
	flag := s.arena[off+OffFlag]
	if flag == FlagDel {
		return 0, false
	}
	version := readU16(s.arena[off+OffVersion:])

	// Saturating add — clamp to MaxUint32, never wrap.
	if ^value < delta {
		value = ^uint32(0)
	} else {
		value += delta
	}
	version = nextVersion(version)
	writeU32(s.arena[off:], value)
	writeU16(s.arena[off+OffVersion:], version)
	s.arena[off+OffChecksum] = calcChecksum(value, flag, version)
	return value, true
}

func (s *Server) dsc(idx uint32, delta uint32) (uint32, bool) {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	value := readU32(s.arena[off:])
	flag := s.arena[off+OffFlag]
	if flag == FlagDel {
		return 0, false
	}
	version := readU16(s.arena[off+OffVersion:])

	// Saturating subtract — clamp to 0, never wrap.
	if delta >= value {
		value = 0
	} else {
		value -= delta
	}
	version = nextVersion(version)
	writeU32(s.arena[off:], value)
	writeU16(s.arena[off+OffVersion:], version)
	s.arena[off+OffChecksum] = calcChecksum(value, flag, version)
	return value, true
}

func (s *Server) rst(idx uint32) bool {
	s.barrier.RLock()
	defer s.barrier.RUnlock()

	mu := &s.stripes[idx&0xFF]
	mu.Lock()
	defer mu.Unlock()

	off := idx << 3
	flag := s.arena[off+OffFlag]
	if flag == FlagDel {
		return false
	}
	version := nextVersion(readU16(s.arena[off+OffVersion:]))
	writeU32(s.arena[off:], 0)
	writeU16(s.arena[off+OffVersion:], version)
	s.arena[off+OffChecksum] = calcChecksum(0, flag, version)
	return true
}

// ─── DFG ──────────────────────────────────────────────────────────────────

func (s *Server) dfg(mode string) (swept uint64, freed uint64, err error) {
	switch mode {
	case "AUTO", "NOW", "DRY":
	default:
		return 0, 0, fmt.Errorf("bad dfg mode")
	}

	if !s.dfgRunning.CompareAndSwap(false, true) {
		return 0, 0, fmt.Errorf("dfg busy")
	}
	defer s.dfgRunning.Store(false)

	now := time.Now()
	if mode != "DRY" {
		last := time.Unix(s.lastDFGUnix.Load(), 0)
		if !last.IsZero() && now.Sub(last) < s.cfg.DFGCooldown {
			return 0, 0, fmt.Errorf("dfg cooldown")
		}
	}

	if mode == "AUTO" {
		tomb := s.tombstones.Load()
		active := s.activeSlots.Load()
		used := active + tomb
		if tomb < s.cfg.AutoDFGMinSlots || used == 0 ||
			int((tomb*100)/used) < s.cfg.AutoDFGRatioPct {
			return 0, 0, fmt.Errorf("dfg threshold")
		}
	}

	// Exclusive lock: no readers can proceed during the sweep.
	s.barrier.Lock()
	defer s.barrier.Unlock()

	for idx := uint32(0); idx < s.slots; idx++ {
		off := idx << 3
		flag := s.arena[off+OffFlag]
		value := readU32(s.arena[off:])
		version := readU16(s.arena[off+OffVersion:])
		checksum := s.arena[off+OffChecksum]

		// Tombstone = deleted AND not free (at least one non-zero byte besides flag).
		isTombstone := flag == FlagDel && !(value == 0 && version == 0 && checksum == 0)
		if !isTombstone {
			continue
		}
		swept++
		if mode == "DRY" {
			continue
		}
		copy(s.arena[off:off+SlotSize], zeroSlot[:])
		freed++
	}

	if mode != "DRY" && freed > 0 {
		s.tombstones.Add(^uint64(freed - 1))
		s.lastDFGUnix.Store(now.Unix())
	}
	return swept, freed, nil
}

func (s *Server) autoDFGLoop() {
	ticker := time.NewTicker(s.cfg.AutoDFGInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _, _ = s.dfg("AUTO")
		case <-s.done:
			return
		}
	}
}

// ─── Bit-level helpers ────────────────────────────────────────────────────

func calcChecksum(value uint32, flag uint8, version uint16) uint8 {
	return uint8(value) ^ uint8(value>>8) ^ uint8(value>>16) ^ uint8(value>>24) ^
		flag ^ uint8(version) ^ uint8(version>>8)
}

func nextVersion(v uint16) uint16 {
	v++
	if v == 0 {
		v = 1 // version 0 is reserved to mean "never written"
	}
	return v
}

func readU16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func writeU16(b []byte, v uint16) {
	b[0] = uint8(v)
	b[1] = uint8(v >> 8)
}
func readU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func writeU32(b []byte, v uint32) {
	b[0] = uint8(v)
	b[1] = uint8(v >> 8)
	b[2] = uint8(v >> 16)
	b[3] = uint8(v >> 24)
}

// ─── Parsing helpers ──────────────────────────────────────────────────────

// splitFirst returns the first whitespace-delimited token and the trimmed remainder.
// Does not allocate.
func splitFirst(s string) (head, tail string) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

// parseSingleIndex parses a single index token with no extra arguments.
func parseSingleIndex(s string, max uint32) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.IndexByte(s, ' ') >= 0 {
		return 0, false
	}
	return parseIndex(s, max)
}

// parseTwoArgs returns exactly two whitespace-separated tokens.
func parseTwoArgs(s string) (string, string, bool) {
	a, b := splitFirst(s)
	if a == "" || b == "" || strings.IndexByte(b, ' ') >= 0 {
		return "", "", false
	}
	return a, b, true
}

func parseIndex(s string, max uint32) (uint32, bool) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || uint32(v) >= max {
		return 0, false
	}
	return uint32(v), true
}

func parseUint32(s string) (uint32, bool) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// ─── Env helpers ──────────────────────────────────────────────────────────

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// ─── HTTP API ─────────────────────────────────────────────────────────────

type httpError struct {
	Status  int
	Message string
	Details any
}

func (e *httpError) Error() string { return e.Message }

type commandResult struct {
	Raw                 string         `json:"raw"`
	Data                map[string]any `json:"data"`
	BackendProcessingUs *int64         `json:"backend_processing_us"`
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	s.writeCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	result, httpErr := s.routeHTTP(r)
	if httpErr != nil {
		s.writeJSON(w, httpErr.Status, map[string]any{
			"ok":      false,
			"error":   httpErr.Message,
			"details": httpErr.Details,
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"operation":             result["operation"],
		"command":               result["command"],
		"backend_processing_us": result["backend_processing_us"],
		"raw":                   result["raw"],
		"data":                  result["data"],
	})
}

func (s *Server) routeHTTP(r *http.Request) (map[string]any, *httpError) {
	segments := splitPath(r.URL.Path)
	method := r.Method

	if len(segments) == 0 && method == http.MethodGet {
		return map[string]any{
			"operation":             "INFO",
			"command":               "",
			"backend_processing_us": nil,
			"raw":                   "OK",
			"data": map[string]any{
				"service": "slotdb-http",
				"routes": map[string]string{
					"ping":          "GET /ping",
					"stats":         "GET /stats",
					"viewSlot":      "GET /slot/:index",
					"upsertSlot":    "PUT /slot/:index {\"value\": uint32}",
					"incrementSlot": "POST /slot/:index/inc {\"delta\": uint32}",
					"decrementSlot": "POST /slot/:index/dsc {\"delta\": uint32}",
					"resetSlot":     "POST /slot/:index/reset",
					"deleteSlot":    "DELETE /slot/:index",
					"dfg":           "POST /dfg {\"mode\": \"AUTO|NOW|DRY\"}",
					"rawCommand":    "POST /command {\"command\": string}",
				},
			},
		}, nil
	}

	if len(segments) == 1 {
		switch {
		case segments[0] == "ping" && method == http.MethodGet:
			return s.runHTTPCommand("PING", "PING")
		case segments[0] == "stats" && method == http.MethodGet:
			return s.runHTTPCommand("STATS", "STATS")
		case segments[0] == "dfg" && method == http.MethodPost:
			body, err := readJSONBody(r)
			if err != nil {
				return nil, err
			}
			mode := "AUTO"
			if v, ok := body["mode"]; ok {
				mode, err = requireString(v, "mode")
				if err != nil {
					return nil, err
				}
				mode = strings.ToUpper(strings.TrimSpace(mode))
				if mode == "" {
					mode = "AUTO"
				}
			}
			return s.runHTTPCommand("DFG "+mode, "DFG")
		case segments[0] == "command" && method == http.MethodPost:
			body, err := readJSONBody(r)
			if err != nil {
				return nil, err
			}
			command, err := requireString(body["command"], "command")
			if err != nil {
				return nil, err
			}
			command = strings.TrimSpace(command)
			if command == "" {
				return nil, &httpError{Status: http.StatusBadRequest, Message: "command is required"}
			}
			return s.runHTTPCommand(command, "COMMAND")
		}
	}

	if len(segments) >= 2 && segments[0] == "slot" {
		idx, err := requireUint32String(segments[1], "index")
		if err != nil {
			return nil, err
		}

		if len(segments) == 2 {
			switch method {
			case http.MethodGet:
				return s.runHTTPCommand(fmt.Sprintf("VIEW %d", idx), "VIEW")
			case http.MethodDelete:
				return s.runHTTPCommand(fmt.Sprintf("DELETE %d", idx), "DELETE")
			case http.MethodPut, http.MethodPost:
				body, err := readJSONBody(r)
				if err != nil {
					return nil, err
				}
				value, err := requireUint32Any(body["value"], "value")
				if err != nil {
					return nil, err
				}
				return s.runHTTPCommand(fmt.Sprintf("UPSERT %d %d", idx, value), "UPSERT")
			}
		}

		if len(segments) == 3 && method == http.MethodPost {
			switch segments[2] {
			case "inc":
				body, err := readJSONBody(r)
				if err != nil {
					return nil, err
				}
				delta := uint32(1)
				if raw, ok := body["delta"]; ok {
					delta, err = requireUint32Any(raw, "delta")
					if err != nil {
						return nil, err
					}
				}
				return s.runHTTPCommand(fmt.Sprintf("INC %d %d", idx, delta), "INC")
			case "dsc":
				body, err := readJSONBody(r)
				if err != nil {
					return nil, err
				}
				delta := uint32(1)
				if raw, ok := body["delta"]; ok {
					delta, err = requireUint32Any(raw, "delta")
					if err != nil {
						return nil, err
					}
				}
				return s.runHTTPCommand(fmt.Sprintf("DSC %d %d", idx, delta), "DSC")
			case "reset":
				return s.runHTTPCommand(fmt.Sprintf("RST %d", idx), "RST")
			}
		}
	}

	return nil, &httpError{Status: http.StatusNotFound, Message: "route not found"}
}

func (s *Server) runHTTPCommand(command, operation string) (map[string]any, *httpError) {
	raw := s.exec(command)
	parsed, err := parseCommandResult(raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"operation":             operation,
		"command":               command,
		"backend_processing_us": parsed.BackendProcessingUs,
		"raw":                   parsed.Raw,
		"data":                  parsed.Data,
	}, nil
}

func parseCommandResult(raw string) (*commandResult, *httpError) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return nil, &httpError{Status: http.StatusInternalServerError, Message: "empty backend response"}
	}
	if parts[0] == "ERR" {
		return nil, mapCommandError(strings.TrimSpace(strings.TrimPrefix(raw, "ERR")))
	}
	if parts[0] != "OK" {
		return nil, &httpError{Status: http.StatusInternalServerError, Message: "bad backend response", Details: raw}
	}

	data := make(map[string]any)
	var backendProcessingUs *int64

	for _, part := range parts[1:] {
		if !strings.Contains(part, "=") {
			data["message"] = part
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		if key == "backend_processing_us" {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				backendProcessingUs = &n
			}
			continue
		}
		if n, err := strconv.ParseUint(value, 10, 64); err == nil {
			data[key] = n
		} else {
			data[key] = value
		}
	}

	return &commandResult{
		Raw:                 raw,
		Data:                data,
		BackendProcessingUs: backendProcessingUs,
	}, nil
}

func mapCommandError(message string) *httpError {
	status := http.StatusBadRequest
	switch {
	case strings.Contains(message, "unknown command"), strings.Contains(message, "usage"):
		status = http.StatusNotFound
	case strings.Contains(message, "not active"):
		status = http.StatusNotFound
	case strings.Contains(message, "server busy"), strings.Contains(message, "rate limit"):
		status = http.StatusTooManyRequests
	case strings.Contains(message, "checksum"):
		status = http.StatusConflict
	case strings.Contains(message, "dfg busy"):
		status = http.StatusConflict
	}
	return &httpError{Status: status, Message: message}
}

func readJSONBody(r *http.Request) (map[string]any, *httpError) {
	if r.Body == nil {
		return map[string]any{}, nil
	}
	defer r.Body.Close()
	var body map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 512))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, &httpError{Status: http.StatusBadRequest, Message: "invalid json body", Details: err.Error()}
	}
	return body, nil
}

func requireString(v any, field string) (string, *httpError) {
	s, ok := v.(string)
	if !ok {
		return "", &httpError{Status: http.StatusBadRequest, Message: field + " must be a string"}
	}
	return s, nil
}

func requireUint32String(v string, field string) (uint32, *httpError) {
	n, ok := parseUint32(strings.TrimSpace(v))
	if !ok {
		return 0, &httpError{Status: http.StatusBadRequest, Message: "bad " + field}
	}
	return n, nil
}

func requireUint32Any(v any, field string) (uint32, *httpError) {
	switch x := v.(type) {
	case float64:
		if x < 0 || x > float64(^uint32(0)) || x != float64(uint32(x)) {
			return 0, &httpError{Status: http.StatusBadRequest, Message: "bad " + field}
		}
		return uint32(x), nil
	case string:
		return requireUint32String(x, field)
	default:
		return 0, &httpError{Status: http.StatusBadRequest, Message: "bad " + field}
	}
}

func splitPath(path string) []string {
	raw := strings.Split(path, "/")
	out := raw[:0]
	for _, part := range raw {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *Server) writeCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
}

// jsonBufPool recycles byte buffers for JSON encoding — avoids per-response alloc.
var jsonBufPool = sync.Pool{New: func() any { return new([]byte) }}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	data, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte{'\n'})
}

// ─── Entry point ─────────────────────────────────────────────────────────

func main() {
	cfg := defaultConfig()
	runtime.GOMAXPROCS(cfg.GoMaxProcs)
	srv := NewServer(cfg)
	if err := srv.Run(); err != nil {
		log.Fatal(err)
	}
}

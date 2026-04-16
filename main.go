package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ============================================================
// CONFIG
// ============================================================

type config struct {
	threads   int
	mode1     bool
	mode2     bool
	server    string
	port      int
	backup    string
	silent    bool
	timeout   int
	batchSize int
}

const (
	POSSIBLE        = "0123456789abcdef"
	LAST_KEY_FILE   = "last_key.txt"
	BACKUP_KEY_FILE = "last_key.bak"
	DEFAULT_TIMEOUT = 10
)

var (
	counter   uint64
	startTime time.Time
	silent    bool
	wg        sync.WaitGroup

	lastKeyMu sync.Mutex
	lastKeyCh = make(chan string, 1)
)

func parseConfig() *config {
	var cfg config
	flag.IntVar(&cfg.threads, "threads", runtime.NumCPU(), "Jumlah thread")
	flag.BoolVar(&cfg.mode1, "mode1", false, "Mode random: generate private key acak")
	flag.BoolVar(&cfg.mode2, "mode2", false, "Mode berurutan: lanjut dari last_key.txt")
	flag.StringVar(&cfg.server, "server", "202.61.239.89", "RPC server utama")
	flag.IntVar(&cfg.port, "port", 8545, "Port RPC server utama")
	flag.StringVar(&cfg.backup, "backup", "", "Backup RPC (pisahkan koma: \"http://s2:8545,http://s3:8545\")")
	flag.BoolVar(&cfg.silent, "silent", false, "Mode diam: hanya tampilkan [STATS] dan [FOUND]")
	flag.IntVar(&cfg.timeout, "timeout", DEFAULT_TIMEOUT, "Timeout per request RPC (detik)")
	flag.IntVar(&cfg.batchSize, "batch", 20, "Jumlah address per batch RPC request")
	flag.Parse()
	return &cfg
}

// ============================================================
// RPC POOL — round-robin + failover + health check + auto-reconnect
// ============================================================

type clientEntry struct {
	eth     *ethclient.Client
	raw     *rpc.Client
	url     string
	healthy int32
	mu      sync.Mutex
}

func (e *clientEntry) reconnect() {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, err := rpc.Dial(e.url)
	if err != nil {
		log.Printf("[RECONNECT] Gagal reconnect ke %s: %v\n", e.url, err)
		return
	}
	e.raw = raw
	e.eth = ethclient.NewClient(raw)
	atomic.StoreInt32(&e.healthy, 1)
	log.Printf("[RECONNECT] Berhasil reconnect ke %s\n", e.url)
}

type rpcPool struct {
	clients   []clientEntry
	rrCounter uint64
}

func newRPCPool(primaryURL string, backupURLs []string) (*rpcPool, error) {
	pool := &rpcPool{}

	raw, err := rpc.Dial(primaryURL)
	if err != nil {
		return nil, fmt.Errorf("gagal sambung ke server utama %s: %w", primaryURL, err)
	}
	pool.clients = append(pool.clients, clientEntry{
		eth: ethclient.NewClient(raw), raw: raw, url: primaryURL, healthy: 1,
	})
	fmt.Printf("[RPC] Server utama  : %s\n", primaryURL)

	for _, url := range backupURLs {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		r, err := rpc.Dial(url)
		if err != nil {
			fmt.Printf("[RPC] Backup gagal  : %s (%v) — dilewati\n", url, err)
			continue
		}
		pool.clients = append(pool.clients, clientEntry{
			eth: ethclient.NewClient(r), raw: r, url: url, healthy: 1,
		})
		fmt.Printf("[RPC] Backup aktif  : %s\n", url)
	}

	fmt.Printf("[RPC] Total server  : %d\n", len(pool.clients))
	pool.startHealthCheck(10)
	return pool, nil
}

func (p *rpcPool) startHealthCheck(intervalSec int) {
	go func() {
		ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
		for range ticker.C {
			for i := range p.clients {
				e := &p.clients[i]
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := e.eth.ChainID(ctx)
				cancel()
				if err != nil {
					if atomic.CompareAndSwapInt32(&e.healthy, 1, 0) {
						log.Printf("[HEALTH] Server DOWN: %s — reconnect...\n", e.url)
					}
					go e.reconnect()
				} else {
					if atomic.CompareAndSwapInt32(&e.healthy, 0, 1) {
						log.Printf("[HEALTH] Server UP: %s\n", e.url)
					}
				}
			}
		}
	}()
}

func (p *rpcPool) getEntry() (*clientEntry, uint64) {
	n := uint64(len(p.clients))
	start := atomic.AddUint64(&p.rrCounter, 1) % n
	for i := uint64(0); i < n; i++ {
		idx := (start + i) % n
		if atomic.LoadInt32(&p.clients[idx].healthy) == 1 {
			return &p.clients[idx], idx
		}
	}
	return &p.clients[start], start
}

func (p *rpcPool) nextEntry(failedIdx uint64) *clientEntry {
	n := uint64(len(p.clients))
	for i := uint64(1); i < n; i++ {
		idx := (failedIdx + i) % n
		if atomic.LoadInt32(&p.clients[idx].healthy) == 1 {
			return &p.clients[idx]
		}
	}
	return &p.clients[(failedIdx+1)%n]
}

// batchGetBalances mengirim N address dalam SATU HTTP request ke RPC
func (p *rpcPool) batchGetBalances(ctx context.Context, addrs []string, timeoutSec int) ([]*big.Int, uint64, error) {
	entry, idx := p.getEntry()

	elems := make([]rpc.BatchElem, len(addrs))
	results := make([]*hexutil.Big, len(addrs))
	for i, addr := range addrs {
		results[i] = new(hexutil.Big)
		elems[i] = rpc.BatchElem{
			Method: "eth_getBalance",
			Args:   []interface{}{addr, "latest"},
			Result: results[i],
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	err := entry.raw.BatchCallContext(callCtx, elems)
	if err != nil {
		// Coba server lain
		if len(p.clients) > 1 {
			other := p.nextEntry(idx)
			callCtx2, cancel2 := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
			defer cancel2()
			err = other.raw.BatchCallContext(callCtx2, elems)
		}
		if err != nil {
			return nil, idx, err
		}
	}

	balances := make([]*big.Int, len(addrs))
	for i, elem := range elems {
		if elem.Error != nil || results[i] == nil {
			balances[i] = big.NewInt(0)
		} else {
			balances[i] = results[i].ToInt()
		}
	}
	return balances, idx, nil
}

// ============================================================
// LAST KEY
// ============================================================

const defaultKey = "0000000000000000000000000000000000000000000000000000000000000001"

func isValidHexKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if strings.IndexByte(POSSIBLE, key[i]) == -1 {
			return false
		}
	}
	return true
}

func resetLastKey(alasan string) string {
	fmt.Printf("[PERINGATAN] last_key.txt %s\n", alasan)
	fmt.Printf("[RESET] Memulai ulang dari key default: %s\n", defaultKey)
	writeLastKey(defaultKey)
	return defaultKey
}

func readLastKey() string {
	data, err := os.ReadFile(LAST_KEY_FILE)
	if err != nil {
		fmt.Println("[INFO] last_key.txt tidak ditemukan. Membuat file baru.")
		writeLastKey(defaultKey)
		return defaultKey
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return resetLastKey("kosong.")
	}
	if len(key) != 64 {
		return resetLastKey(fmt.Sprintf("tidak valid: panjang %d karakter (harus 64).", len(key)))
	}
	if !isValidHexKey(key) {
		return resetLastKey("mengandung karakter yang bukan hex (0-9, a-f).")
	}
	fmt.Printf("[INFO] Melanjutkan dari key: %s\n", key)
	return key
}

func writeLastKey(key string) {
	lastKeyMu.Lock()
	defer lastKeyMu.Unlock()
	if err := os.WriteFile(LAST_KEY_FILE, []byte(key), 0644); err != nil {
		log.Printf("Gagal menyimpan last_key.txt: %v\n", err)
	}
}

func startKeyWriter() {
	go func() {
		for key := range lastKeyCh {
			writeLastKey(key)
		}
	}()
}

func sendLastKey(key string) {
	select {
	case lastKeyCh <- key:
	default:
		select {
		case <-lastKeyCh:
		default:
		}
		lastKeyCh <- key
	}
}

func backupLastKey() {
	lastKeyMu.Lock()
	data, err := os.ReadFile(LAST_KEY_FILE)
	lastKeyMu.Unlock()
	if err != nil {
		return
	}
	if err = os.WriteFile(BACKUP_KEY_FILE, data, 0644); err != nil {
		log.Printf("Backup: gagal tulis %s: %v\n", BACKUP_KEY_FILE, err)
		return
	}
	fmt.Printf("[BACKUP] last_key.bak diperbarui → %s\n", strings.TrimSpace(string(data)))
}

func startBackupRoutine(intervalMenit int) {
	fmt.Printf("[BACKUP] Backup otomatis aktif setiap %d menit ke last_key.bak\n", intervalMenit)
	go func() {
		ticker := time.NewTicker(time.Duration(intervalMenit) * time.Minute)
		for range ticker.C {
			backupLastKey()
		}
	}()
}

// ============================================================
// KEY GENERATION
// ============================================================

func generateNextPrivKey(privHex string) string {
	b := []byte(privHex)
	for i := len(b) - 1; i >= 0; i-- {
		pos := strings.IndexByte(POSSIBLE, b[i])
		if pos == 15 {
			b[i] = '0'
		} else {
			b[i] = POSSIBLE[pos+1]
			break
		}
	}
	return string(b)
}

// generateRandomPrivKey menggunakan rand lokal — tidak ada mutex, tidak ada contention
func generateRandomPrivKey(r *rand.Rand) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = POSSIBLE[r.Intn(16)]
	}
	return string(b)
}

func generateAddressFromPrivKey(privHex string) string {
	privateKey, err := crypto.HexToECDSA(privHex)
	if err != nil {
		log.Fatal(err)
	}
	pubKey, ok := privateKey.Public().(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}
	return crypto.PubkeyToAddress(*pubKey).Hex()
}

// ============================================================
// BALANCE CHECK — batch RPC, retry, graceful shutdown
// ============================================================

func checkBalance(ctx context.Context, data chan string, pool *rpcPool, timeoutSec int, batchSize int) {
	defer wg.Done()

	const (
		minBackoff    = 500 * time.Millisecond
		maxBackoff    = 10 * time.Second
		flushInterval = 100 * time.Millisecond
	)

	batch := make([]string, 0, batchSize)
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()

	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		addrs := make([]string, len(batch))
		for i, cred := range batch {
			parts := strings.SplitN(cred, ":", 2)
			if len(parts) == 2 {
				addrs[i] = parts[1]
			}
		}

		attempt := 0
		for {
			select {
			case <-ctx.Done():
				return false
			default:
			}

			balances, idx, err := pool.batchGetBalances(ctx, addrs, timeoutSec)
			if err != nil {
				attempt++
				if attempt%3 == 0 {
					go pool.clients[idx].reconnect()
				}
				shift := attempt
				if shift > 5 {
					shift = 5
				}
				backoff := minBackoff * (1 << shift)
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				log.Printf("[RETRY] attempt=%d backoff=%s err=%v\n", attempt, backoff, err)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return false
				}
				continue
			}

			for i, bal := range balances {
				if bal != nil && bal.Cmp(big.NewInt(0)) != 0 {
					found := batch[i] + ":" + bal.String() + "\n"
					writeToFound(found, "found.txt")
					fmt.Printf("[FOUND] %s\n", found)
				}
			}
			n := uint64(len(batch))
			atomic.AddUint64(&counter, n)
			if !silent {
				for i, cred := range batch {
					if balances[i] != nil {
						fmt.Printf("Creds: %s Balance: %s Counter: %d\n",
							cred, balances[i].String(), atomic.LoadUint64(&counter))
					}
				}
			}
			break
		}
		batch = batch[:0]
		return true
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case cred, ok := <-data:
			if !ok {
				flush()
				return
			}
			batch = append(batch, cred)
			if len(batch) >= batchSize {
				if !flush() {
					return
				}
				timer.Reset(flushInterval)
			}
		case <-timer.C:
			if !flush() {
				return
			}
			timer.Reset(flushInterval)
		}
	}
}

// ============================================================
// OUTPUT & STATS
// ============================================================

func writeToFound(text string, path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0655)
	if err != nil {
		log.Fatalf("Open file: %s %v\n", text, err)
	}
	defer f.Close()
	if _, err = f.WriteString(text); err != nil {
		log.Fatalf("Write string: %s %v\n", text, err)
	}
}

func writeStatsLog(line string) {
	f, err := os.OpenFile("stats.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}

func startSpeedStats(intervalSec int) {
	writeStatsLog(fmt.Sprintf("\n=== Sesi dimulai: %s ===", startTime.Format("2006-01-02 15:04:05")))
	go func() {
		ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
		var lastCount uint64
		for range ticker.C {
			current := atomic.LoadUint64(&counter)
			elapsed := time.Since(startTime)
			keysPerSec := float64(current-lastCount) / float64(intervalSec)
			avgPerSec := float64(current) / elapsed.Seconds()
			line := fmt.Sprintf("[%s] Elapsed: %s | Total: %d | Speed: %.1f keys/s | Avg: %.1f keys/s",
				time.Now().Format("15:04:05"), elapsed.Round(time.Second), current, keysPerSec, avgPerSec)
			fmt.Printf("\n[STATS] %s\n\n", line)
			writeStatsLog(line)
			lastCount = current
		}
	}()
}

func cleanup() {
	elapsed := time.Since(startTime)
	total := atomic.LoadUint64(&counter)
	avgPerSec := float64(total) / elapsed.Seconds()
	line := fmt.Sprintf("[SELESAI] Total: %d alamat | Waktu: %s | Rata-rata: %.1f keys/s",
		total, elapsed.Round(time.Second), avgPerSec)
	fmt.Printf("\n%s\n", line)
	writeStatsLog(line)
}

// ============================================================
// MAIN
// ============================================================

func main() {
	cfg := parseConfig()

	if !cfg.mode1 && !cfg.mode2 {
		flag.Usage()
		os.Exit(1)
	}
	if cfg.mode1 && cfg.mode2 {
		fmt.Fprintln(os.Stderr, "Pilih salah satu: -mode1 atau -mode2")
		os.Exit(1)
	}

	silent = cfg.silent
	if silent {
		fmt.Println("[SILENT] Mode diam aktif — hanya tampil [STATS] dan [FOUND]")
	}

	primaryURL := fmt.Sprintf("http://%s:%d", cfg.server, cfg.port)
	var backupURLs []string
	if cfg.backup != "" {
		backupURLs = strings.Split(cfg.backup, ",")
	}

	pool, err := newRPCPool(primaryURL, backupURLs)
	if err != nil {
		log.Fatalf("%v\n", err)
	}

	fmt.Printf("[BATCH] Ukuran batch  : %d address/request\n", cfg.batchSize)

	startTime = time.Now()
	// Buffer besar: generator tidak pernah nunggu worker
	chData := make(chan string, cfg.threads*50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chExit := make(chan os.Signal, 1)
	signal.Notify(chExit, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-chExit
		fmt.Println("\n[SHUTDOWN] Sinyal diterima — menyelesaikan proses yang berjalan...")
		cancel()
	}()

	startSpeedStats(60)

	if cfg.mode1 {
		fmt.Printf("[MODE1] Random | Threads: %d | Batch: %d | Timeout: %ds | Server: %d\n",
			cfg.threads, cfg.batchSize, cfg.timeout, len(pool.clients))
		for t := 0; t < cfg.threads; t++ {
			wg.Add(1)
			go checkBalance(ctx, chData, pool, cfg.timeout, cfg.batchSize)
		}
		// Rand lokal per producer — tidak ada mutex, tidak ada contention
		localR := rand.New(rand.NewSource(time.Now().UnixNano()))
	outer1:
		for {
			pk := generateRandomPrivKey(localR)
			select {
			case chData <- pk + ":" + generateAddressFromPrivKey(pk):
			case <-ctx.Done():
				break outer1
			}
		}

	} else {
		startKeyWriter()
		startBackupRoutine(5)
		pk := readLastKey()
		fmt.Printf("[MODE2] Berurutan | Mulai: %s | Threads: %d | Batch: %d | Timeout: %ds | Server: %d\n",
			pk, cfg.threads, cfg.batchSize, cfg.timeout, len(pool.clients))
		for t := 0; t < cfg.threads; t++ {
			wg.Add(1)
			go checkBalance(ctx, chData, pool, cfg.timeout, cfg.batchSize)
		}
	outer2:
		for {
			pk = generateNextPrivKey(pk)
			select {
			case chData <- pk + ":" + generateAddressFromPrivKey(pk):
				sendLastKey(pk)
			case <-ctx.Done():
				break outer2
			}
		}
	}

	close(chData)
	wg.Wait()
	cleanup()
	fmt.Println("[SHUTDOWN] Selesai. Semua proses telah dihentikan dengan aman.")
}

package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"io"
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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ============================================================
// CONFIG
// ============================================================

type config struct {
	threads int
	mode1   bool
	mode2   bool
	server  string
	port    int
	backup  string
	silent  bool
	timeout int
}

const (
	POSSIBLE        = "0123456789abcdef"
	LAST_KEY_FILE   = "last_key.txt"
	BACKUP_KEY_FILE = "last_key.bak"
	DEFAULT_TIMEOUT = 5
)

var (
	counter   uint64
	startTime time.Time
	silent    bool
	wg        sync.WaitGroup

	randMu    sync.Mutex
	globalR   = rand.New(rand.NewSource(time.Now().UnixNano()))
	lastKeyMu sync.Mutex
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
	flag.Parse()
	return &cfg
}

// ============================================================
// RPC POOL — round-robin otomatis + failover
// ============================================================

type clientEntry struct {
	client *ethclient.Client
	url    string
}

type rpcPool struct {
	clients   []clientEntry
	rrCounter uint64
}

func newRPCPool(primaryURL string, backupURLs []string) (*rpcPool, error) {
	pool := &rpcPool{}

	c, err := ethclient.Dial(primaryURL)
	if err != nil {
		return nil, fmt.Errorf("gagal sambung ke server utama %s: %w", primaryURL, err)
	}
	pool.clients = append(pool.clients, clientEntry{client: c, url: primaryURL})
	fmt.Printf("[RPC] Server utama  : %s\n", primaryURL)

	for _, url := range backupURLs {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		bc, err := ethclient.Dial(url)
		if err != nil {
			fmt.Printf("[RPC] Backup gagal  : %s (%v) — dilewati\n", url, err)
			continue
		}
		pool.clients = append(pool.clients, clientEntry{client: bc, url: url})
		fmt.Printf("[RPC] Backup aktif  : %s\n", url)
	}

	fmt.Printf("[RPC] Total server  : %d\n", len(pool.clients))
	return pool, nil
}

// getClient round-robin: setiap request dikirim ke server berbeda secara bergilir
func (p *rpcPool) getClient() (*ethclient.Client, uint64) {
	idx := atomic.AddUint64(&p.rrCounter, 1) % uint64(len(p.clients))
	return p.clients[idx].client, idx
}

// nextClient ambil server berikutnya setelah error (skip server yang gagal)
func (p *rpcPool) nextClient(failedIdx uint64) *ethclient.Client {
	idx := (failedIdx + 1) % uint64(len(p.clients))
	return p.clients[idx].client
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
		fmt.Printf("[INFO] Mulai dari key default: %s\n", defaultKey)
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

func backupLastKey() {
	lastKeyMu.Lock()
	data, err := os.ReadFile(LAST_KEY_FILE)
	lastKeyMu.Unlock()
	if err != nil {
		log.Printf("Backup: gagal baca %s: %v\n", LAST_KEY_FILE, err)
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

// generateNextPrivKey menaikkan hex key satu angka (pakai []byte, tanpa alokasi ekstra)
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

// generateRandomPrivKey membuat hex key 64 karakter secara acak (pakai []byte)
func generateRandomPrivKey() string {
	b := make([]byte, 64)
	randMu.Lock()
	for i := range b {
		b[i] = POSSIBLE[globalR.Intn(16)]
	}
	randMu.Unlock()
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
// BALANCE CHECK — timeout + round-robin failover
// ============================================================

func balanceAt(client *ethclient.Client, address string, timeoutSec int) (*big.Int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	balance, err := client.BalanceAt(ctx, common.HexToAddress(address), nil)
	if err != nil {
		if err == io.EOF {
			log.Fatalf("Check balance fatal: %s %v\n", address, err)
		}
		return nil, err
	}
	return balance, nil
}

func checkBalance(data chan string, pool *rpcPool, timeoutSec int) {
	defer wg.Done()
	for creds := range data {
		parts := strings.SplitN(creds, ":", 2)
		if len(parts) < 2 {
			continue
		}
		addr := parts[1]

		client, idx := pool.getClient()
		balance, err := balanceAt(client, addr, timeoutSec)
		if err != nil {
			// Jika ada server lain, langsung retry ke server berikutnya
			if len(pool.clients) > 1 {
				balance, err = balanceAt(pool.nextClient(idx), addr, timeoutSec)
			}
			if err != nil {
				log.Printf("[ERROR] %s: %v\n", pool.clients[idx].url, err)
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		if balance.Cmp(big.NewInt(0)) != 0 {
			found := creds + ":" + balance.String() + "\n"
			writeToFound(found, "found.txt")
			fmt.Printf("[FOUND] %s\n", found)
		}

		atomic.AddUint64(&counter, 1)
		if !silent {
			fmt.Printf("Creds: %s Balance: %s Counter: %d\n",
				creds, balance.String(), atomic.LoadUint64(&counter))
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
		log.Printf("Stats log: %v\n", err)
		return
	}
	defer f.Close()
	if _, err = f.WriteString(line + "\n"); err != nil {
		log.Printf("Stats log write: %v\n", err)
	}
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

	startTime = time.Now()
	chData := make(chan string, cfg.threads*2)
	chExit := make(chan os.Signal, 1)
	signal.Notify(chExit, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-chExit
		cleanup()
		os.Exit(0)
	}()

	startSpeedStats(60)

	if cfg.mode1 {
		fmt.Printf("[MODE1] Random | Threads: %d | Timeout: %ds | Server: %d\n",
			cfg.threads, cfg.timeout, len(pool.clients))
		for t := 0; t < cfg.threads; t++ {
			wg.Add(1)
			go checkBalance(chData, pool, cfg.timeout)
		}
		for {
			pk := generateRandomPrivKey()
			chData <- pk + ":" + generateAddressFromPrivKey(pk)
		}

	} else {
		startBackupRoutine(5)
		pk := readLastKey()
		fmt.Printf("[MODE2] Berurutan | Mulai: %s | Threads: %d | Timeout: %ds | Server: %d\n",
			pk, cfg.threads, cfg.timeout, len(pool.clients))
		for t := 0; t < cfg.threads; t++ {
			wg.Add(1)
			go checkBalance(chData, pool, cfg.timeout)
		}
		for {
			pk = generateNextPrivKey(pk)
			chData <- pk + ":" + generateAddressFromPrivKey(pk)
			go writeLastKey(pk)
		}
	}
}

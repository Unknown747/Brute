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
        counter   uint64 = 0
        startTime time.Time
        silent    bool
        wg        sync.WaitGroup
        usage     = func() {
                fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
                fmt.Fprintf(os.Stderr, "  Mode random   : ./eth-brute -threads 50 -mode1\n")
                fmt.Fprintf(os.Stderr, "  Mode berurutan: ./eth-brute -threads 50 -mode2\n")
                fmt.Fprintf(os.Stderr, "  Multi-server  : ./eth-brute -threads 50 -mode1 -backup \"http://server2:8545,http://server3:8545\"\n\n")
                flag.PrintDefaults()
        }

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
        flag.StringVar(&cfg.backup, "backup", "", "Backup RPC servers (pisahkan dengan koma, contoh: \"http://server2:8545,http://server3:8545\")")
        flag.BoolVar(&cfg.silent, "silent", false, "Mode diam: hanya tampilkan [STATS] dan [FOUND]")
        flag.IntVar(&cfg.timeout, "timeout", DEFAULT_TIMEOUT, "Timeout per request RPC (detik)")
        flag.Parse()
        return &cfg
}

// ============================================================
// RPC POOL — multi-server dengan failover otomatis
// ============================================================

type rpcPool struct {
        clients []clientEntry
        current uint64
}

type clientEntry struct {
        client *ethclient.Client
        url    string
}

func newRPCPool(primaryURL string, backupURLs []string) (*rpcPool, error) {
        pool := &rpcPool{}

        // Sambungkan server utama
        c, err := ethclient.Dial(primaryURL)
        if err != nil {
                return nil, fmt.Errorf("gagal sambung ke server utama %s: %w", primaryURL, err)
        }
        pool.clients = append(pool.clients, clientEntry{client: c, url: primaryURL})
        fmt.Printf("[RPC] Server utama  : %s\n", primaryURL)

        // Sambungkan server backup
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

// getClient mengembalikan client aktif beserta indeksnya
func (p *rpcPool) getClient() (*ethclient.Client, uint64) {
        idx := atomic.LoadUint64(&p.current) % uint64(len(p.clients))
        return p.clients[idx].client, idx
}

// rotate pindah ke server berikutnya saat error
func (p *rpcPool) rotate(failedIdx uint64) {
        if len(p.clients) <= 1 {
                return
        }
        next := (failedIdx + 1) % uint64(len(p.clients))
        if atomic.CompareAndSwapUint64(&p.current, failedIdx, next) {
                fmt.Printf("\n[RPC] Pindah ke server: %s\n", p.clients[next].url)
        }
}

// watchPrimary mencoba kembali ke server utama setiap 30 detik
func (p *rpcPool) watchPrimary() {
        if len(p.clients) <= 1 {
                return
        }
        go func() {
                ticker := time.NewTicker(30 * time.Second)
                for range ticker.C {
                        current := atomic.LoadUint64(&p.current) % uint64(len(p.clients))
                        if current == 0 {
                                continue // sudah di server utama
                        }
                        // Coba ping server utama
                        ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
                        _, err := p.clients[0].client.BlockNumber(ctx)
                        cancel()
                        if err == nil {
                                atomic.StoreUint64(&p.current, 0)
                                fmt.Printf("\n[RPC] Kembali ke server utama: %s\n", p.clients[0].url)
                        }
                }
        }()
}

// ============================================================
// LAST KEY
// ============================================================

const defaultKey = "0000000000000000000000000000000000000000000000000000000000000001"

func isValidHexKey(key string) bool {
        if len(key) != 64 {
                return false
        }
        for _, c := range key {
                if !strings.ContainsRune(POSSIBLE, c) {
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
                fmt.Printf("[INFO] last_key.txt tidak ditemukan. Membuat file baru.\n")
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
        ticker := time.NewTicker(time.Duration(intervalMenit) * time.Minute)
        fmt.Printf("[BACKUP] Backup otomatis aktif setiap %d menit ke last_key.bak\n", intervalMenit)
        go func() {
                for range ticker.C {
                        backupLastKey()
                }
        }()
}

// ============================================================
// KEY GENERATION
// ============================================================

func generateNextPrivKey(privHex string) string {
        sh := strings.Split(privHex, "")
        for i := len(privHex) - 1; i >= 0; i-- {
                point := strings.Index(POSSIBLE, sh[i])
                if point == 15 {
                        sh[i] = "0"
                } else {
                        sh[i] = string(POSSIBLE[point+1])
                        break
                }
        }
        return strings.Join(sh, "")
}

func generateRandomPrivKey() string {
        randMu.Lock()
        defer randMu.Unlock()
        var randHex string
        for c := 0; c < 64; c++ {
                randHex += string(POSSIBLE[globalR.Intn(16)])
        }
        return randHex
}

func generateAddressFromPrivKey(privHex string) string {
        privateKey, err := crypto.HexToECDSA(privHex)
        if err != nil {
                log.Fatal(err)
        }
        publicKeyECDSA, ok := privateKey.Public().(*ecdsa.PublicKey)
        if !ok {
                log.Fatal("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
        }
        return crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
}

// ============================================================
// BALANCE CHECK — timeout + failover
// ============================================================

func balanceAt(client *ethclient.Client, address string, timeoutSec int) (*big.Int, error) {
        ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
        defer cancel()
        account := common.HexToAddress(address)
        balance, err := client.BalanceAt(ctx, account, nil)
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
                        log.Printf("[ERROR] %s: %v\n", pool.clients[idx].url, err)
                        pool.rotate(idx)
                        time.Sleep(200 * time.Millisecond)
                        continue
                }

                if balance.Cmp(big.NewInt(0)) != 0 {
                        found := creds + ":" + balance.String() + "\n"
                        writeToFound(found, "found.txt")
                        // [FOUND] selalu tampil meski -silent aktif
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
// OUTPUT
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
        f.WriteString(line + "\n")
}

func startSpeedStats(intervalSec int) {
        ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
        var lastCount uint64 = 0
        writeStatsLog(fmt.Sprintf("\n=== Sesi dimulai: %s ===", startTime.Format("2006-01-02 15:04:05")))
        go func() {
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
                usage()
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

        // Bangun RPC pool
        primaryURL := fmt.Sprintf("http://%s:%d", cfg.server, cfg.port)
        var backupURLs []string
        if cfg.backup != "" {
                backupURLs = strings.Split(cfg.backup, ",")
        }

        pool, err := newRPCPool(primaryURL, backupURLs)
        if err != nil {
                log.Fatalf("%v\n", err)
        }
        pool.watchPrimary()

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
                fmt.Printf("[MODE1] Random | Threads: %d | Timeout: %ds\n", cfg.threads, cfg.timeout)
                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(chData, pool, cfg.timeout)
                }
                for {
                        pk := generateRandomPrivKey()
                        addr := generateAddressFromPrivKey(pk)
                        chData <- fmt.Sprintf("%s:%s", pk, addr)
                }

        } else if cfg.mode2 {
                if cfg.mode2 {
                        startBackupRoutine(5)
                }
                pk := readLastKey()
                fmt.Printf("[MODE2] Berurutan | Mulai: %s | Threads: %d | Timeout: %ds\n",
                        pk, cfg.threads, cfg.timeout)
                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(chData, pool, cfg.timeout)
                }
                for {
                        pk = generateNextPrivKey(pk)
                        addr := generateAddressFromPrivKey(pk)
                        chData <- fmt.Sprintf("%s:%s", pk, addr)
                        go writeLastKey(pk)
                }
        }
}

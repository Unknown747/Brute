package main

import (
        "bytes"
        "bufio"
        "context"
        "crypto/ecdsa"
        crand "crypto/rand"
        "encoding/binary"
        "encoding/json"
        "flag"
        "fmt"
        "log"
        "math/big"
        "math/rand"
        "net/http"
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
// CONSTANTS
// ============================================================

const (
        POSSIBLE        = "0123456789abcdef"
        LAST_KEY_FILE   = "last_key.txt"
        BACKUP_KEY_FILE = "last_key.bak"
        DEFAULT_TIMEOUT = 10

        // ===== GANTI DI SINI KALAU MAU PAKAI RPC LAIN =====
        DEFAULT_RPC = "https://eth.llamarpc.com"
        // ===================================================

        batchMin      = 5
        batchMax      = 100
        batchAdjEvery = 10
)

// ============================================================
// CONFIG
// ============================================================

type config struct {
        threads   int
        producers int
        mode1     bool
        mode2     bool
        rpc       string
        server    string
        port      int
        backup    string
        silent    bool
        timeout   int
        batchSize int
}

func parseConfig() *config {
        var cfg config
        flag.IntVar(&cfg.threads, "threads", runtime.NumCPU(), "Jumlah thread checker")
        flag.IntVar(&cfg.producers, "producers", runtime.NumCPU(), "Jumlah producer goroutine (mode1)")
        flag.BoolVar(&cfg.mode1, "mode1", false, "Mode random: generate private key acak")
        flag.BoolVar(&cfg.mode2, "mode2", false, "Mode berurutan: lanjut dari last_key.txt")
        flag.StringVar(&cfg.rpc, "rpc", "", "URL RPC lengkap (contoh: http://ip:8545)")
        flag.StringVar(&cfg.server, "server", "", "RPC server host (alternatif dari -rpc)")
        flag.IntVar(&cfg.port, "port", 8545, "Port RPC server")
        flag.StringVar(&cfg.backup, "backup", "", "Backup RPC, pisahkan koma: \"http://s2:8545,http://s3:8545\"")
        flag.BoolVar(&cfg.silent, "silent", false, "Mode diam: hanya tampilkan [STATS] dan [FOUND]")
        flag.IntVar(&cfg.timeout, "timeout", DEFAULT_TIMEOUT, "Timeout per request RPC (detik)")
        flag.IntVar(&cfg.batchSize, "batch", 20, "Ukuran batch awal (adaptive, min 5 max 100)")
        flag.Parse()
        return &cfg
}

// resolveRPC menentukan URL RPC utama: -rpc > -server/-port > DEFAULT_RPC
func resolveRPC(cfg *config) string {
        if cfg.rpc != "" {
                return cfg.rpc
        }
        if cfg.server != "" {
                return fmt.Sprintf("http://%s:%d", cfg.server, cfg.port)
        }
        return DEFAULT_RPC
}

// ============================================================
// GLOBALS
// ============================================================

var (
        counter   uint64
        startTime time.Time
        silent    bool
        wg        sync.WaitGroup

        lastKeyMu sync.Mutex
        lastKeyCh = make(chan string, 1)

        foundMu     sync.Mutex
        foundWriter *bufio.Writer
        foundFile   *os.File

        statsMu     sync.Mutex
        statsWriter *bufio.Writer
        statsFile   *os.File

        tgCfg *telegramConfig
)

// ============================================================
// TELEGRAM NOTIFICATION
// ============================================================

type telegramConfig struct {
        BotToken string `json:"bot_token"`
        ChatID   string `json:"chat_id"`
}

func loadTelegramConfig(path string) *telegramConfig {
        data, err := os.ReadFile(path)
        if err != nil {
                return nil
        }
        var cfg telegramConfig
        if err := json.Unmarshal(data, &cfg); err != nil {
                log.Printf("[TELEGRAM] Gagal parse %s: %v — dinonaktifkan\n", path, err)
                return nil
        }
        // Validasi format: bot token Telegram selalu mengandung ":"
        if cfg.BotToken == "" || cfg.ChatID == "" || !strings.Contains(cfg.BotToken, ":") {
                log.Printf("[TELEGRAM] telegram.json belum dikonfigurasi — dinonaktifkan\n")
                return nil
        }
        log.Printf("[TELEGRAM] Notifikasi aktif → chat_id: %s\n", cfg.ChatID)
        return &cfg
}

func sendTelegram(msg string) {
        if tgCfg == nil {
                return
        }
        go func() {
                url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", tgCfg.BotToken)
                body, _ := json.Marshal(map[string]string{
                        "chat_id": tgCfg.ChatID,
                        "text":    msg,
                })
                resp, err := http.Post(url, "application/json", bytes.NewReader(body))
                if err != nil {
                        log.Printf("[TELEGRAM] Gagal kirim: %v\n", err)
                        return
                }
                defer resp.Body.Close()
                if resp.StatusCode != http.StatusOK {
                        log.Printf("[TELEGRAM] Status tidak OK: %d\n", resp.StatusCode)
                }
        }()
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
                log.Printf("[RECONNECT] Gagal ke %s: %v\n", e.url, err)
                return
        }
        e.raw = raw
        e.eth = ethclient.NewClient(raw)
        atomic.StoreInt32(&e.healthy, 1)
        log.Printf("[RECONNECT] Berhasil ke %s\n", e.url)
}

type rpcPool struct {
        clients   []clientEntry
        rrCounter uint64
}

func newRPCPool(primary string, backups []string) (*rpcPool, error) {
        pool := &rpcPool{}

        raw, err := rpc.Dial(primary)
        if err != nil {
                return nil, fmt.Errorf("gagal sambung ke server utama %s: %w", primary, err)
        }
        pool.clients = append(pool.clients, clientEntry{
                eth: ethclient.NewClient(raw), raw: raw, url: primary, healthy: 1,
        })
        fmt.Printf("[RPC] Server utama : %s\n", primary)

        for _, u := range backups {
                u = strings.TrimSpace(u)
                if u == "" {
                        continue
                }
                r, err := rpc.Dial(u)
                if err != nil {
                        fmt.Printf("[RPC] Backup gagal : %s (%v) — dilewati\n", u, err)
                        continue
                }
                pool.clients = append(pool.clients, clientEntry{
                        eth: ethclient.NewClient(r), raw: r, url: u, healthy: 1,
                })
                fmt.Printf("[RPC] Backup aktif : %s\n", u)
        }

        fmt.Printf("[RPC] Total server : %d\n", len(pool.clients))
        return pool, nil
}

func isRateLimit(err error) bool {
        if err == nil {
                return false
        }
        s := err.Error()
        return strings.Contains(s, "429") ||
                strings.Contains(s, "Too Many Requests") ||
                strings.Contains(s, "rate limit") ||
                strings.Contains(s, "rate_limited")
}

func (p *rpcPool) startHealthCheck(ctx context.Context, intervalSec int) {
        go func() {
                ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
                defer ticker.Stop()
                for {
                        select {
                        case <-ctx.Done():
                                return
                        case <-ticker.C:
                        }
                        for i := range p.clients {
                                e := &p.clients[i]
                                hCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                                _, err := e.eth.ChainID(hCtx)
                                cancel()
                                if err != nil {
                                        if isRateLimit(err) {
                                                continue
                                        }
                                        if atomic.CompareAndSwapInt32(&e.healthy, 1, 0) {
                                                log.Printf("[HEALTH] DOWN: %s — reconnect...\n", e.url)
                                        }
                                        go e.reconnect()
                                } else {
                                        if atomic.CompareAndSwapInt32(&e.healthy, 0, 1) {
                                                log.Printf("[HEALTH] UP: %s\n", e.url)
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
        err := entry.raw.BatchCallContext(callCtx, elems)
        cancel() // segera bebaskan context setelah selesai

        if err != nil && len(p.clients) > 1 {
                other := p.nextEntry(idx)
                callCtx2, cancel2 := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
                err = other.raw.BatchCallContext(callCtx2, elems)
                cancel2() // segera bebaskan context setelah selesai
        }

        if err != nil {
                return nil, idx, err
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
// LAST KEY (mode2)
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

func readLastKey() string {
        data, err := os.ReadFile(LAST_KEY_FILE)
        if err != nil {
                fmt.Println("[INFO] last_key.txt tidak ditemukan — mulai dari default.")
                writeLastKey(defaultKey)
                return defaultKey
        }
        key := strings.TrimSpace(string(data))
        if key == "" || len(key) != 64 || !isValidHexKey(key) {
                fmt.Printf("[RESET] last_key.txt tidak valid — mulai dari default.\n")
                writeLastKey(defaultKey)
                return defaultKey
        }
        fmt.Printf("[INFO] Melanjutkan dari key: %s\n", key)
        return key
}

func writeLastKey(key string) {
        lastKeyMu.Lock()
        defer lastKeyMu.Unlock()
        if err := os.WriteFile(LAST_KEY_FILE, []byte(key), 0644); err != nil {
                log.Printf("Gagal simpan last_key.txt: %v\n", err)
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
        // Selalu non-blocking: coba kirim langsung; jika channel penuh,
        // drain dulu lalu coba kirim lagi — semua cabang tidak pernah blocking.
        select {
        case lastKeyCh <- key:
        default:
                // Kosongkan slot yang sudah ada (key lama)
                select {
                case <-lastKeyCh:
                default:
                }
                // Kirim key baru; jika slot sudah diambil writer, pakai default
                select {
                case lastKeyCh <- key:
                default:
                }
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
                log.Printf("Backup gagal: %v\n", err)
                return
        }
        fmt.Printf("[BACKUP] last_key.bak → %s\n", strings.TrimSpace(string(data)))
}

func startBackupRoutine(ctx context.Context, intervalMenit int) {
        fmt.Printf("[BACKUP] Backup otomatis setiap %d menit\n", intervalMenit)
        go func() {
                ticker := time.NewTicker(time.Duration(intervalMenit) * time.Minute)
                defer ticker.Stop()
                for {
                        select {
                        case <-ctx.Done():
                                return
                        case <-ticker.C:
                                backupLastKey()
                        }
                }
        }()
}

// ============================================================
// KEY GENERATION
// ============================================================

func generateNextPrivKey(privHex string) string {
        b := []byte(privHex)
        incremented := false
        for i := len(b) - 1; i >= 0; i-- {
                pos := strings.IndexByte(POSSIBLE, b[i])
                if pos == 15 {
                        b[i] = '0'
                } else {
                        b[i] = POSSIBLE[pos+1]
                        incremented = true
                        break
                }
        }
        // If every digit was 'f', the key wrapped to all-zeros which is an invalid
        // secp256k1 private key — reset to the first valid key instead.
        if !incremented {
                return defaultKey
        }
        return string(b)
}

// cryptoRandSeed menghasilkan seed 64-bit yang benar-benar acak menggunakan
// crypto/rand, sehingga setiap restart program tidak mengulang urutan yang sama.
func cryptoRandSeed() int64 {
        var buf [8]byte
        if _, err := crand.Read(buf[:]); err != nil {
                // Fallback ke waktu jika crypto/rand gagal (sangat jarang terjadi)
                log.Printf("[WARN] crypto/rand gagal, pakai time seed: %v\n", err)
                return time.Now().UnixNano()
        }
        return int64(binary.LittleEndian.Uint64(buf[:]))
}

func generateRandomPrivKey(r *rand.Rand) string {
        b := make([]byte, 64)
        for i := range b {
                b[i] = POSSIBLE[r.Intn(16)]
        }
        return string(b)
}

func generateAddressFromPrivKey(privHex string) (string, error) {
        privateKey, err := crypto.HexToECDSA(privHex)
        if err != nil {
                return "", fmt.Errorf("HexToECDSA(%s): %w", privHex, err)
        }
        pubKey, ok := privateKey.Public().(*ecdsa.PublicKey)
        if !ok {
                return "", fmt.Errorf("publicKey bukan *ecdsa.PublicKey untuk key %s", privHex)
        }
        return crypto.PubkeyToAddress(*pubKey).Hex(), nil
}

// ============================================================
// ADAPTIVE BATCH SIZE
// ============================================================

type adaptiveBatch struct {
        size     int64
        attempts uint64
        success  uint64
}

func newAdaptiveBatch(initial int) *adaptiveBatch {
        if initial < batchMin {
                initial = batchMin
        }
        if initial > batchMax {
                initial = batchMax
        }
        return &adaptiveBatch{size: int64(initial)}
}

func (a *adaptiveBatch) get() int {
        return int(atomic.LoadInt64(&a.size))
}

func (a *adaptiveBatch) record(ok bool) {
        atomic.AddUint64(&a.attempts, 1)
        if ok {
                atomic.AddUint64(&a.success, 1)
        }
        total := atomic.LoadUint64(&a.attempts)
        if total%batchAdjEvery != 0 {
                return
        }
        succ := atomic.LoadUint64(&a.success)
        rate := float64(succ) / float64(total)
        cur := atomic.LoadInt64(&a.size)

        if rate > 0.90 {
                next := cur + 5
                if next > batchMax {
                        next = batchMax
                }
                if next != cur && atomic.CompareAndSwapInt64(&a.size, cur, next) {
                        log.Printf("[BATCH] Naik %d→%d (sukses %.0f%%)\n", cur, next, rate*100)
                }
        } else if rate < 0.70 {
                next := cur - 5
                if next < batchMin {
                        next = batchMin
                }
                if next != cur && atomic.CompareAndSwapInt64(&a.size, cur, next) {
                        log.Printf("[BATCH] Turun %d→%d (sukses %.0f%%)\n", cur, next, rate*100)
                }
        }
}

// ============================================================
// BALANCE CHECK
// ============================================================

// onChecked dipanggil setelah satu batch berhasil dicek; argumennya adalah
// private key TERAKHIR dalam batch. Di mode2 ini dipakai untuk menyimpan
// progress ke last_key.txt SETELAH kunci benar-benar diperiksa — bukan
// hanya setelah masuk channel buffer.
func checkBalance(ctx context.Context, data chan string, pool *rpcPool, timeoutSec int, ab *adaptiveBatch, onChecked func(string)) {
        defer wg.Done()

        const (
                minBackoff    = 500 * time.Millisecond
                maxBackoff    = 10 * time.Second
                flushInterval = 100 * time.Millisecond
        )

        batch := make([]string, 0, ab.get())
        timer := time.NewTimer(flushInterval)
        defer timer.Stop()

        flush := func() bool {
                if len(batch) == 0 {
                        return true
                }
                addrs := make([]string, len(batch))
                for i, cred := range batch {
                        if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
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
                                ab.record(false)
                                attempt++

                                // Deteksi rate limit 429 — tunggu lebih lama
                                var backoff time.Duration
                                if isRateLimit(err) {
                                        backoff = 30 * time.Second
                                        log.Printf("[RATELIMIT] Kena rate limit, tunggu %s...\n", backoff)
                                } else {
                                        if attempt%3 == 0 {
                                                go pool.clients[idx].reconnect()
                                        }
                                        shift := attempt
                                        if shift > 5 {
                                                shift = 5
                                        }
                                        backoff = minBackoff * (1 << shift)
                                        if backoff > maxBackoff {
                                                backoff = maxBackoff
                                        }
                                        log.Printf("[RETRY] attempt=%d backoff=%s err=%v\n", attempt, backoff, err)
                                }

                                select {
                                case <-time.After(backoff):
                                case <-ctx.Done():
                                        return false
                                }
                                continue
                        }

                        ab.record(true)
                        for i, bal := range balances {
                                if bal != nil && bal.Cmp(big.NewInt(0)) != 0 {
                                        found := batch[i] + ":" + bal.String()
                                        writeToFound(found + "\n")
                                        fmt.Printf("[FOUND] %s\n", found)
                                        sendTelegram(fmt.Sprintf("🎉 [FOUND]\n%s", found))
                                }
                        }
                        atomic.AddUint64(&counter, uint64(len(batch)))
                        if !silent {
                                for i, cred := range batch {
                                        if balances[i] != nil {
                                                fmt.Printf("Creds: %s Balance: %s Counter: %d\n",
                                                        cred, balances[i].String(), atomic.LoadUint64(&counter))
                                        }
                                }
                        }
                        // Setelah batch berhasil dicek, beritahu caller (mode2)
                        // key terakhir yang sudah dikonfirmasi diperiksa.
                        if onChecked != nil && len(batch) > 0 {
                                lastCred := batch[len(batch)-1]
                                if parts := strings.SplitN(lastCred, ":", 2); len(parts) == 2 {
                                        onChecked(parts[0])
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
                        if len(batch) >= ab.get() {
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

// openFoundFile membuka found.txt sekali di awal program.
// Menggunakan bufio.Writer agar tidak ada syscall open/close tiap baris.
func openFoundFile() {
        f, err := os.OpenFile("found.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
        if err != nil {
                log.Fatalf("Gagal buka found.txt: %v\n", err)
        }
        foundFile = f
        foundWriter = bufio.NewWriter(f)
}

// closeFoundFile flush buffer dan tutup file saat shutdown.
func closeFoundFile() {
        foundMu.Lock()
        defer foundMu.Unlock()
        if foundWriter != nil {
                _ = foundWriter.Flush()
        }
        if foundFile != nil {
                _ = foundFile.Close()
        }
}

func writeToFound(text string) {
        foundMu.Lock()
        defer foundMu.Unlock()
        if foundWriter == nil {
                return
        }
        if _, err := foundWriter.WriteString(text); err != nil {
                log.Printf("Gagal tulis found.txt: %v\n", err)
                return
        }
        // Langsung flush agar data tidak hilang jika program tiba-tiba berhenti
        _ = foundWriter.Flush()
}

func openStatsFile() {
        f, err := os.OpenFile("stats.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
        if err != nil {
                log.Printf("[WARN] Gagal buka stats.log: %v\n", err)
                return
        }
        statsFile = f
        statsWriter = bufio.NewWriter(f)
}

func closeStatsFile() {
        statsMu.Lock()
        defer statsMu.Unlock()
        if statsWriter != nil {
                _ = statsWriter.Flush()
        }
        if statsFile != nil {
                _ = statsFile.Close()
        }
}

func writeStatsLog(line string) {
        statsMu.Lock()
        defer statsMu.Unlock()
        if statsWriter == nil {
                return
        }
        _, _ = statsWriter.WriteString(line + "\n")
        _ = statsWriter.Flush()
}

func startSpeedStats(ctx context.Context, intervalSec int) {
        writeStatsLog(fmt.Sprintf("\n=== Sesi dimulai: %s ===", startTime.Format("2006-01-02 15:04:05")))
        go func() {
                ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
                defer ticker.Stop()
                var lastCount uint64
                for {
                        select {
                        case <-ctx.Done():
                                return
                        case <-ticker.C:
                        }
                        current := atomic.LoadUint64(&counter)
                        elapsed := time.Since(startTime)
                        speed := float64(current-lastCount) / float64(intervalSec)
                        avg := float64(current) / elapsed.Seconds()
                        line := fmt.Sprintf("[%s] Elapsed: %s | Total: %d | Speed: %.1f keys/s | Avg: %.1f keys/s",
                                time.Now().Format("15:04:05"), elapsed.Round(time.Second), current, speed, avg)
                        fmt.Printf("\n[STATS] %s\n\n", line)
                        writeStatsLog(line)
                        lastCount = current
                }
        }()
}

func cleanup() {
        elapsed := time.Since(startTime)
        total := atomic.LoadUint64(&counter)
        avg := float64(total) / elapsed.Seconds()
        line := fmt.Sprintf("[SELESAI] Total: %d alamat | Waktu: %s | Rata-rata: %.1f keys/s",
                total, elapsed.Round(time.Second), avg)
        fmt.Printf("\n%s\n", line)
        writeStatsLog(line)
        closeFoundFile()
        closeStatsFile()
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
                fmt.Println("[SILENT] Mode diam — hanya tampil [STATS] dan [FOUND]")
        }

        openFoundFile()
        openStatsFile()

        tgCfg = loadTelegramConfig("telegram.json")

        pool, err := newRPCPool(resolveRPC(cfg), func() []string {
                if cfg.backup == "" {
                        return nil
                }
                return strings.Split(cfg.backup, ",")
        }())
        if err != nil {
                log.Fatalf("%v\n", err)
        }

        ab := newAdaptiveBatch(cfg.batchSize)
        fmt.Printf("[BATCH] Ukuran batch : %d (adaptive, min %d max %d)\n", ab.get(), batchMin, batchMax)

        startTime = time.Now()
        chData := make(chan string, cfg.threads*50)

        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        pool.startHealthCheck(ctx, 10)

        chExit := make(chan os.Signal, 1)
        signal.Notify(chExit, os.Interrupt, syscall.SIGTERM)
        go func() {
                <-chExit
                fmt.Println("\n[SHUTDOWN] Sinyal diterima — menyelesaikan proses...")
                cancel()
        }()

        startSpeedStats(ctx, 60)

        if cfg.mode1 {
                fmt.Printf("[MODE1] Random | Checkers: %d | Producers: %d | Batch: %d | Timeout: %ds | Server: %d\n",
                        cfg.threads, cfg.producers, ab.get(), cfg.timeout, len(pool.clients))

                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(ctx, chData, pool, cfg.timeout, ab, nil)
                }

                var prodWg sync.WaitGroup
                for p := 0; p < cfg.producers; p++ {
                        prodWg.Add(1)
                        go func() {
                                defer prodWg.Done()
                                r := rand.New(rand.NewSource(cryptoRandSeed()))
                                for {
                                        pk := generateRandomPrivKey(r)
                                        addr, err := generateAddressFromPrivKey(pk)
                                        if err != nil {
                                                log.Printf("[SKIP] key tidak valid, dilewati: %v\n", err)
                                                continue
                                        }
                                        select {
                                        case chData <- pk + ":" + addr:
                                        case <-ctx.Done():
                                                return
                                        }
                                }
                        }()
                }
                prodWg.Wait()

        } else {
                startKeyWriter()
                startBackupRoutine(ctx, 5)
                pk := readLastKey()
                fmt.Printf("[MODE2] Berurutan | Mulai: %s | Checkers: %d | Batch: %d | Timeout: %ds | Server: %d\n",
                        pk, cfg.threads, ab.get(), cfg.timeout, len(pool.clients))

                // onChecked: simpan progress SETELAH key benar-benar dicek,
                // bukan hanya setelah masuk channel buffer.
                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(ctx, chData, pool, cfg.timeout, ab, func(checkedPK string) {
                                sendLastKey(checkedPK)
                        })
                }
        outer:
                for {
                        pk = generateNextPrivKey(pk)
                        addr, err := generateAddressFromPrivKey(pk)
                        if err != nil {
                                log.Printf("[SKIP] key tidak valid, dilewati: %v\n", err)
                                continue
                        }
                        select {
                        case chData <- pk + ":" + addr:
                        case <-ctx.Done():
                                break outer
                        }
                }
                close(lastKeyCh) // hentikan goroutine writer dengan bersih
        }

        close(chData)
        wg.Wait()
        cleanup()
        fmt.Println("[SHUTDOWN] Selesai.")
}

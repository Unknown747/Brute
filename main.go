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
        "strconv"
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
}

const (
        POSSIBLE       = "0123456789abcdef"
        LAST_KEY_FILE  = "last_key.txt"
        BACKUP_KEY_FILE = "last_key.bak"
)

var (
        counter   uint64 = 0
        startTime time.Time
        wg        sync.WaitGroup
        usage     = func() {
                fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
                fmt.Fprintf(os.Stderr, "  Mode random  : ./eth-brute -threads 50 -mode1\n")
                fmt.Fprintf(os.Stderr, "  Mode berurutan: ./eth-brute -threads 50 -mode2\n\n")
                flag.PrintDefaults()
        }

        randMu  sync.Mutex
        globalR = rand.New(rand.NewSource(time.Now().UnixNano()))

        lastKeyMu sync.Mutex
)

func parseConfig() *config {
        var cfg config

        flag.IntVar(&cfg.threads, "threads", runtime.NumCPU(), "Jumlah thread")
        flag.BoolVar(&cfg.mode1, "mode1", false, "Mode random: generate private key acak")
        flag.BoolVar(&cfg.mode2, "mode2", false, "Mode berurutan: lanjut dari last_key.txt")
        flag.StringVar(&cfg.server, "server", "202.61.239.89", "Ethereum rpc server")
        flag.IntVar(&cfg.port, "port", 8545, "Ethereum rpc port")
        flag.Parse()

        return &cfg
}

const defaultKey = "0000000000000000000000000000000000000000000000000000000000000001"

// isValidHexKey memeriksa apakah string adalah hex 64 karakter yang valid
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

// resetLastKey menghapus file lama, menulis ulang dengan default key, dan memberi tahu pengguna
func resetLastKey(alasan string) string {
        fmt.Printf("[PERINGATAN] last_key.txt %s\n", alasan)
        fmt.Printf("[RESET] Memulai ulang dari key default: %s\n", defaultKey)
        writeLastKey(defaultKey)
        return defaultKey
}

// readLastKey membaca private key dari last_key.txt dengan validasi penuh
// Jika file tidak ada, kosong, rusak, atau formatnya salah → reset otomatis ke default
func readLastKey() string {
        data, err := os.ReadFile(LAST_KEY_FILE)
        if err != nil {
                // File tidak ditemukan
                fmt.Printf("[INFO] last_key.txt tidak ditemukan. Membuat file baru.\n")
                writeLastKey(defaultKey)
                fmt.Printf("[INFO] Mulai dari key default: %s\n", defaultKey)
                return defaultKey
        }

        key := strings.TrimSpace(string(data))

        // File kosong
        if key == "" {
                return resetLastKey("kosong.")
        }

        // Panjang tidak sesuai
        if len(key) != 64 {
                return resetLastKey(fmt.Sprintf("tidak valid: panjang %d karakter (harus 64).", len(key)))
        }

        // Karakter bukan hex
        if !isValidHexKey(key) {
                return resetLastKey("mengandung karakter yang bukan hex (0-9, a-f).")
        }

        fmt.Printf("[INFO] Melanjutkan dari key: %s\n", key)
        return key
}

// Simpan private key terbaru ke last_key.txt
func writeLastKey(key string) {
        lastKeyMu.Lock()
        defer lastKeyMu.Unlock()
        err := os.WriteFile(LAST_KEY_FILE, []byte(key), 0644)
        if err != nil {
                log.Printf("Gagal menyimpan last_key.txt: %v\n", err)
        }
}

// backupLastKey menyalin isi last_key.txt ke last_key.bak
func backupLastKey() {
        lastKeyMu.Lock()
        data, err := os.ReadFile(LAST_KEY_FILE)
        lastKeyMu.Unlock()

        if err != nil {
                log.Printf("Backup: gagal baca %s: %v\n", LAST_KEY_FILE, err)
                return
        }

        err = os.WriteFile(BACKUP_KEY_FILE, data, 0644)
        if err != nil {
                log.Printf("Backup: gagal tulis %s: %v\n", BACKUP_KEY_FILE, err)
                return
        }

        key := strings.TrimSpace(string(data))
        fmt.Printf("[BACKUP] last_key.bak diperbarui → %s\n", key)
}

// startBackupRoutine menjalankan backup last_key.txt setiap intervalMenit menit
func startBackupRoutine(intervalMenit int) {
        ticker := time.NewTicker(time.Duration(intervalMenit) * time.Minute)
        fmt.Printf("[BACKUP] Backup otomatis aktif setiap %d menit ke last_key.bak\n", intervalMenit)

        go func() {
                for range ticker.C {
                        backupLastKey()
                }
        }()
}

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
                n := globalR.Intn(16)
                randHex += string(POSSIBLE[n])
        }
        return randHex
}

func balanceAt(client *ethclient.Client, address string) (*big.Int, error) {
        account := common.HexToAddress(address)
        balance, err := client.BalanceAt(context.Background(), account, nil)
        if err != nil {
                if err == io.EOF {
                        log.Fatalf("Check balance: %s %v\n", address, err)
                }
                return nil, err
        }
        return balance, nil
}

func generateAddressFromPrivKey(privHex string) string {
        privateKey, err := crypto.HexToECDSA(privHex)
        if err != nil {
                log.Fatal(err)
        }
        publicKey := privateKey.Public()
        publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
        if !ok {
                log.Fatal("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
        }
        return crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
}

func checkBalance(data chan string, client *ethclient.Client) {
        defer wg.Done()

        for creds := range data {
                parts := strings.SplitN(creds, ":", 2)
                if len(parts) < 2 {
                        continue
                }
                addr := parts[1]

                balance, err := balanceAt(client, addr)
                if err != nil {
                        if err == io.EOF {
                                log.Fatalf("Check balance: %s %v\n", creds, err)
                        }
                        log.Printf("Check balance: %s %v\n", creds, err)
                        time.Sleep(500 * time.Millisecond)
                        continue
                }

                if balance.Cmp(big.NewInt(0)) != 0 {
                        found := creds + ":" + balance.String() + "\n"
                        writeToFound(found, "found.txt")
                }
                atomic.AddUint64(&counter, 1)
                fmt.Printf("Creds: %s Balance: %s Counter: %d\n", creds, balance.String(), atomic.LoadUint64(&counter))
        }
}

func writeToFound(text string, path string) {
        f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0655)
        if err != nil {
                log.Fatalf("Open file: %s %v\n", text, err)
        }
        defer f.Close()
        _, err = f.WriteString(text)
        if err != nil {
                log.Fatalf("Write string: %s %v\n", text, err)
        }
}

// writeStatsLog menulis baris stats ke file stats.log
func writeStatsLog(line string) {
        f, err := os.OpenFile("stats.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
        if err != nil {
                log.Printf("Stats log: %v\n", err)
                return
        }
        defer f.Close()
        f.WriteString(line + "\n")
}

// startSpeedStats mencetak stats kecepatan setiap 1 menit dan menyimpan ke stats.log
func startSpeedStats(intervalSec int) {
        ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
        var lastCount uint64 = 0

        sessionStart := fmt.Sprintf("\n=== Sesi dimulai: %s ===", startTime.Format("2006-01-02 15:04:05"))
        writeStatsLog(sessionStart)

        go func() {
                for range ticker.C {
                        current := atomic.LoadUint64(&counter)
                        elapsed := time.Since(startTime)
                        keysPerSec := float64(current-lastCount) / float64(intervalSec)
                        avgPerSec := float64(current) / elapsed.Seconds()

                        line := fmt.Sprintf("[%s] Elapsed: %s | Total: %d | Speed: %.1f keys/s | Avg: %.1f keys/s",
                                time.Now().Format("15:04:05"),
                                elapsed.Round(time.Second), current, keysPerSec, avgPerSec)

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

        client, err := ethclient.Dial("http://" + cfg.server + ":" + strconv.Itoa(cfg.port))
        if err != nil {
                log.Fatalf("Client: %s\n", err)
        }
        defer client.Close()

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

        if cfg.mode2 {
                // Backup last_key.txt setiap 5 menit, hanya aktif di mode berurutan
                startBackupRoutine(5)
        }

        if cfg.mode1 {
                // Mode random
                fmt.Printf("[MODE1] Random | Threads: %d | Server: %s:%d\n", cfg.threads, cfg.server, cfg.port)
                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(chData, client)
                }
                for {
                        pk := generateRandomPrivKey()
                        addr := generateAddressFromPrivKey(pk)
                        chData <- fmt.Sprintf("%s:%s", pk, addr)
                }

        } else if cfg.mode2 {
                // Mode berurutan — lanjut dari last_key.txt, auto update setiap key baru
                pk := readLastKey()
                fmt.Printf("[MODE2] Berurutan | Mulai dari: %s | Threads: %d | Server: %s:%d\n",
                        pk, cfg.threads, cfg.server, cfg.port)

                for t := 0; t < cfg.threads; t++ {
                        wg.Add(1)
                        go checkBalance(chData, client)
                }
                for {
                        pk = generateNextPrivKey(pk)
                        addr := generateAddressFromPrivKey(pk)
                        chData <- fmt.Sprintf("%s:%s", pk, addr)
                        // Simpan progress setiap key baru ke last_key.txt
                        go writeLastKey(pk)
                }
        }
}


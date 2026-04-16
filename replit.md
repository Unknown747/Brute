# eth-brute

## Overview
A Go command-line utility that brute-forces Ethereum private keys and checks for non-zero balances via an Ethereum RPC node.

## Usage Modes
- **Random mode**: `go run . -random` — generates random private keys and checks balances
- **Sequential mode**: `go run . -pk <64-char-hex>` — increments from a starting private key
- **Brain wallet mode**: `go run . -brain <password-file>` — derives keys from SHA256-hashed passwords

## Flags
- `-pk` — Starting private key (64 hex chars)
- `-threads` — Number of concurrent goroutines (default: number of CPUs)
- `-random` — Use random key generation
- `-server` — Ethereum RPC server address (default: `202.61.239.89`)
- `-port` — Ethereum RPC port (default: `8545`)
- `-brain` — Path to a password list file

## Tech Stack
- **Language**: Go 1.16+ (built with go1.21)
- **Key dependency**: `github.com/ethereum/go-ethereum v1.10.17`
- **Package manager**: Go Modules (`go.mod` / `go.sum`)

## Project Structure
```
main.go       # All application logic
go.mod        # Module definition and dependencies
go.sum        # Dependency checksums
eth-brute     # Compiled binary (not committed)
found.txt     # Created at runtime when accounts with balance are found
```

## Build
```bash
go build -o eth-brute .
```

## Workflow
The Replit workflow runs `go run . -h` to display usage information in the console.

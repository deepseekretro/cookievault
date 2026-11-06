# CookieVault Server (Go)

```bash
go build -o cookievault-server ./cmd/server
./cookievault-server -addr 127.0.0.1:18765 -data-dir ./data
```

- 依赖：`github.com/mattn/go-sqlite3`（需 CGO/gcc）、`golang.org/x/crypto`（Argon2id）
- 测试：`go vet ./... && go test ./...`

# CookieVault Server (Go)

```bash
make build          # 推荐：带 FTS5 全文搜索 (-tags sqlite_fts5)
./cookievault-server -addr 127.0.0.1:18765 -data-dir ./data
```

- 依赖：`github.com/mattn/go-sqlite3`（需 CGO/gcc）、`golang.org/x/crypto`（Argon2id）
- 不带 tag 构建（`go build ./cmd/server`）也能运行，搜索自动回退到 LIKE 子串匹配。
- 测试：`make test`（= `go vet` + `go test -tags sqlite_fts5`）

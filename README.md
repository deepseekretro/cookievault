# CookieVault

浏览器 Cookie 按账号快照保存/恢复工具：**Chrome / Edge 插件（MV3 侧边栏）+ Go 本地服务端（SQLite + 主密码加密）**。

> 同一个域名下可能有多个不同账号的登录 Cookie，无法在浏览器里同时共存。
> 本工具把每次保存都做成一个独立快照（必填账号标签），恢复时整组替换，实现账号间切换。

## 仓库结构

```
cookievault/
├── server/     # Go 服务端（单二进制，SQLite）
│   ├── cmd/server/main.go
│   └── internal/{api,crypto,store}
└── extension/  # 浏览器插件（MV3）
    ├── manifest.json
    ├── background.js      # Service Worker: cookie 收集/恢复、调服务端 API
    ├── sidepanel.*        # 侧边栏主界面：虚拟列表 + 无限滚动 + 搜索
    ├── popup.*            # 工具栏小弹窗：快速保存
    └── options.*          # 连接配对 / 主密码解锁
```

## 快速开始

### 1. 启动服务端

```bash
cd server
make build                      # 带 FTS5 全文搜索；不带 tag 也可运行（自动回退 LIKE）
./cookievault-server            # 监听 127.0.0.1:18765，数据在 ./data
```

启动日志会打印 **配对码**（5 分钟有效），记下来。

首次使用设置主密码（至少 8 位），会返回一个 Token：

```bash
curl -X POST http://127.0.0.1:18765/v1/auth/setup \
  -H 'Content-Type: application/json' \
  -d '{"password":"your-master-password"}'
```

### 2. 加载插件

Chrome / Edge → 扩展 → 开发者模式 → **加载已解压的扩展程序** → 选择本仓库的 `extension/` 目录。

### 3. 连接与使用

1. 点插件图标 →「连接 / 解锁设置」：填服务端地址 + 启动日志里的配对码 → 连接成功。
2. 点工具栏图标打开**侧边栏**：顶部搜索框可搜索标签/域名/备注，列表是虚拟列表，滚动到底部自动加载更多。
3. 在某网站登录后，点「＋ 保存当前」，填**账号标签**（如 `张三-工作账号`）保存快照。
4. 需要切换账号时，在列表点「恢复」——默认替换模式：先自动备份当前 Cookie，再清除该域名旧 Cookie 并写入快照 Cookie。

## 安全模型

- 服务端只绑 `127.0.0.1`；校验 Host 和 Origin（只允许插件来源），防 DNS rebinding / CSRF。
- Cookie 值 AES-256-GCM 加密落盘；主密码 Argon2id 派生，信封加密，**遗忘主密码数据不可恢复**。
- 数据接口需要配对 Token + 服务端解锁；解锁失败 5 次后冷却 30 秒。
- 审计/日志永不记录 Cookie 值；插件详情页里 Cookie 值默认脱敏。

## 开发

```bash
cd server && go vet ./... && go test ./...
cd ../extension && node --check background.js sidepanel.js popup.js options.js
```

## P3 已实现

- **Profile 分组浏览**：侧边栏按 Profile 筛选/新建分组，保存时可归入分组，卡片显示分组名
- **快照差异对比**：`GET /v1/snapshots/compare?a=&b=` 返回新增/删除/变化（字段级）/未变，只返回 Cookie 标识，不泄露值
- **单快照加密导出**：`POST /v1/snapshots/{id}/export`，与整库备份同格式，可用备份导入恢复
- **锁定前自动备份**：插件每分钟监控解锁时长，闲置锁定前 1 分钟自动保存当前域名脏 Cookie；手动锁定时也会先备份（可在设置页关闭）

## P2 已实现

- **加密备份导入导出**：备份文件用独立备份密码（Argon2id+AES-GCM）整体加密，可跨机器/换主密码导入；导入按指纹去重
- **闲置自动锁定**：默认 15 分钟无操作自动锁定，可在设置页调整（0=禁用）
- **FTS5 全文搜索**：trigram 索引搜标签/域名/备注（带 `-tags sqlite_fts5` 构建时）
- **合并/替换恢复模式**可按快照保存偏好；操作审计日志（只记元数据）

详细设计见 [DESIGN.md](DESIGN.md)。

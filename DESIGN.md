# CookieVault 设计文档（v4）

## 决策记录
- 存储：SQLite（WAL）；Cookie 值落盘加密
- 密钥：用户主密码，Argon2id + AES-256-GCM 信封加密（改密码只重包 DEK）
- 插件：Chrome / Edge，Manifest V3，侧边栏为主界面
- 保存模式：每次保存新建快照，必填标签；同域名多账号靠快照互斥切换
- 浏览：虚拟列表（定高行）+ 无限滚动（游标分页）+ 顶部搜索框（服务端搜索 + 高亮）

## 架构
```
插件 sidepanel/background ──chrome.cookies API──▶ 浏览器 Cookie
        │  fetch http://127.0.0.1:18765 (Bearer Token)
        ▼
Go 服务端 ── 解密/加密 ──▶ SQLite (value_enc + nonce)
```

## 数据模型
- `profiles(id, name, note)`：可选分组
- `snapshots(id, profile_id, domain, label, note, store_id, is_replace, fingerprint, cookie_count, http_only_count, created_at, last_used_at)`：一次保存 = 一个账号登录状态
- `cookies(id, snapshot_id, name, domain, host_only, path, secure, http_only, same_site, expires_at, partitioned, partition_key, value_enc, nonce, hash)`：值加密，元数据明文以便索引搜索；同快照内按 (name,domain,path,partition) 去重，跨快照不去重
- `meta`：KDF 盐、参数、包装后的 DEK；`tokens`：Token 哈希；`audit_log`：仅元数据

## API（节选）
- `GET /v1/health`、`GET /v1/auth/status`
- `POST /v1/auth/setup|unlock|lock|change-password`
- `POST /v1/pair/verify`（配对码 → Token）、`POST /v1/pair/code`
- `GET/POST /v1/profiles`、`DELETE /v1/profiles/{id}`
- `POST /v1/snapshots`（新建快照，label 必填）
- `GET /v1/snapshots?q=&domain=&profile_id=&sort=&limit=&cursor=` → `{items, next_cursor, has_more, total}`（游标 = base64url("<排序键>\n<id>")）
- `GET/PATCH/DELETE /v1/snapshots/{id}`、`GET /v1/snapshots/{id}/cookies?q=`、`POST /v1/snapshots/{id}/touch`

## 虚拟列表 + 无限滚动
- 快照卡片定高 76px、Cookie 行 44px；只渲染可视区 + overscan(5)，DOM 恒定 ≤ 30 个节点
- 占位层高度 = total × 行高，撑起真实滚动条；rAF 节流滚动渲染
- IntersectionObserver 哨兵 + 距底 400px 预加载，游标续载，客户端 Map 按 id 去重
- 详情页 Cookie 是本地过滤（单快照通常 < 数百条）

## 搜索
- 顶部搜索框防抖 300ms，服务端 LIKE 匹配 label/domain/note，结果 <mark> 高亮
- `/` 聚焦、`Esc` 清空；搜索重置游标与滚动；可叠加"仅当前域名"与排序

## 恢复流程
1. （可选）把当前域名 Cookie 自动存为“自动备份”快照
2. 替换模式：`chrome.cookies.remove` 清除该域名现有 Cookie
3. 逐条 `chrome.cookies.set`（失败时去掉 partitionKey 重试一次），汇总成功/失败数
4. `touch` 快照更新 last_used_at

## P2 增补

- **备份**：`POST /v1/backup/export|import`。备份内容（profiles+snapshots+解码后 Cookie 值）用独立 backup_password 直接加密（Argon2id+AES-GCM，文件不含明文），与 vault DEK 无关，换机器/换主密码可导入。导入时新分配 id、Profile 按 name 复用、按 (domain,label,fingerprint) 去重跳过（`skip_duplicates` 可关）。
- **闲置自动锁定**：`GET/POST /v1/settings {idle_lock_minutes}`（默认 15，0 禁用），服务端每 30s 检查最后一次鉴权请求时间，超时则清 DEK 锁定。
- **FTS5**：`snapshots_fts` trigram 外部内容表 + 触发器同步；≥3 字符 token 走 FTS 前缀 AND，短/CJK 查询回退 LIKE。需 `-tags sqlite_fts5` 构建，未带 tag 时自动回退 LIKE（功能不变）。
- **恢复模式**：替换（默认）/合并，每快照 `is_replace` 偏好可 PATCH 持久化。
- **审计**：snapshot_create/delete/restore_touch/backup_* 写 audit_log，只记元数据。

## P3 增补

- **Profile 分组**：`profile_id` 过滤已在列表接口支持；侧边栏下拉筛选 + 新建，保存时可归组，卡片显示分组名。
- **快照对比**：`GET /v1/snapshots/compare?a=&b=`，按 (name,domain,path,partition) 对齐，返回 added/removed/changed(字段名)/unchanged，永不返回 Cookie 值。侧边栏卡片/详情有对比入口。
- **单快照导出**：`POST /v1/snapshots/{id}/export` 复用备份容器格式（buildPlain 单快照），可用 `/backup/import` 恢复，导入去重同样适用。
- **锁定前自动备份**：插件 background 用 `chrome.alarms` 每分钟轮询 `/auth/status`（该接口不重置服务端闲置计时）；本地记录首次解锁时间，接近 idle 上限（剩 60s）且当前活动域名 Cookie 指纹已变时，自动新建“闲置锁定前自动备份”快照，保存本身会重置服务端计时。手动锁定（设置页）同样先备份。指纹（FNV）在本地计算/存储，不发往服务端。

## 安全
- 只绑 loopback；Host/Origin 白名单；自定义 Header + Bearer Token
- 主密钥只在内存（解锁时），锁定清零；忘记主密码不可恢复
- 不绕过 HttpOnly：只用官方 chrome.cookies API；__Host- 等受浏览器策略限制的 Cookie 恢复可能失败并如实汇报

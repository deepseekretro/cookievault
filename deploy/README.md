# 部署到 OpenResty 虚拟目录

把 Go 服务部署在 `https://edge02.10w123.com/cookievault/` 下，浏览器扩展在
设置面板里把服务端地址填为该 URL 即可。

```
浏览器扩展 ──https──▶ Cloudflare ──▶ OpenResty(443, /cookievault/)
                                          │ proxy_pass (剥前缀)
                                          ▼
                                      Go 服务 127.0.0.1:18765
```

## 1. 安装 Go 服务（systemd）

```bash
go build -tags sqlite_fts5 -o /usr/local/bin/cookievault-server ./cmd/server
useradd -r -s /usr/sbin/nologin cookievault
install -d -o cookievault -g cookievault -m 700 /var/lib/cookievault
cp deploy/systemd/cookievault.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now cookievault
journalctl -u cookievault -f   # 看配对码，首次设置主密码
```

服务以 `-allowed-hosts edge02.10w123.com` 启动，服务端只在 Host 白名单内接受公网域名。

## 2. 配置 OpenResty

把 `deploy/openresty/snippets/cookievault-proxy.conf` 放到
`/usr/local/openresty/nginx/conf/snippets/`，并把
`deploy/openresty/cookievault-location.conf` 的内容粘进
`conf.d/edge02.10w123.com.conf` 的 443（及 8080）server 块中
（`location /` 之前），然后：

```bash
/usr/local/openresty/bin/openresty -t && /usr/local/openresty/bin/openresty -s reload
```

验证：

```bash
curl -k https://edge02.10w123.com/cookievault/v1/health   # {"ok":true,...}
```

## 3. 扩展里设置地址

扩展侧边栏「设置」Tab（或独立设置页）→ 服务端地址填
`https://edge02.10w123.com/cookievault` → 用服务端日志的配对码连接。

## 安全注意事项

- `/cookievault/` 必须关闭 Basic Auth（扩展 fetch 无法处理），鉴权靠
  Bearer Token + 主密码；请勿在该路径下再挂其他服务
- 建议启用 `limit_req` 限流（见 location 配置注释）防暴力破解主密码
- Cookie 值在传输中是明文 JSON，必须走 HTTPS（经 Cloudflare 边缘时为可信证书）；
  不要用 http 明文地址
- Go 服务只监听 loopback，公网无法绕过 OpenResty 直连

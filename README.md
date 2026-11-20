# agentd

Hệ thống multi-agent nhẹ (Go, 1 binary): **Chat Agent** nói chuyện với user → giao việc cho **Worker** → Worker có thể tạo **Subagent**; kết quả đi ngược lên cây. Có SOUL.md, memory (SQLite + FTS5), skills kiểu `SKILL.md`, nhiều model/provider.

## Chạy

```bash
go build -o bin/agentd ./cmd/agentd
./bin/agentd setup           # wizard: API (chính thức / custom kiểu Anthropic / kiểu OpenAI), chọn + test model, Telegram, search
./bin/agentd                 # chat trong terminal
./bin/agentd -telegram       # chạy bot Telegram
```

Mọi thứ nằm trong home của user chạy agentd (giống `~/.hermes`):

```
~/.agentd/          config.yaml, secrets.env (chmod 600), souls/SOUL.md, roles/, skills/, data/ (DB + log)
~/workspace/        nơi worker làm việc; file gửi qua Telegram vào ~/workspace/inbox/<ngày>/
```

`setup` tự tạo `~/.agentd` (soul, roles, skills mặc định được nhúng trong binary), ghi `config.yaml` (bản cũ → `config.yaml.bak`) và `secrets.env`. Đổi thư mục bằng `AGENTD_HOME` hoặc `-config`. Biến môi trường thật được ưu tiên hơn `secrets.env`. Chạy từ thư mục source có sẵn `config.yaml` thì dùng file đó.

Build cho server ARM (Alpine, Raspberry Pi…): `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/agentd-linux-arm64 ./cmd/agentd`

## Chạy bằng Docker

Cần Docker và Docker Compose v2 trở lên. Image hỗ trợ `linux/amd64` và `linux/arm64`, kèm desktop XFCE, Chromium và cua-driver.

```bash
docker compose up -d --build --wait
docker compose exec agentd agentd setup   # nhập API key và chọn model ở lần đầu
docker compose exec agentd agentd         # chat trong terminal
```

Mở [desktop qua noVNC](http://127.0.0.1:6080/vnc.html?autoconnect=true&resize=scale) để sử dụng Chromium và các ứng dụng XFCE. Role `operator` tự có các tool `browser_*` (Chromium qua CDP) và `cua_*` (desktop qua MCP). Nếu một website yêu cầu đăng nhập, bạn có thể tự đăng nhập trong noVNC. Profile Chromium, cấu hình, lịch sử và workspace được giữ trong Docker volumes qua các lần khởi động lại.

Để chạy bot Telegram nền, nhập token và ID người dùng được phép khi chạy `setup`, thêm `AGENTD_MODE=telegram` vào file `.env` cạnh `compose.yaml`, rồi chạy `docker compose up -d --wait`.

- Đổi cổng noVNC bằng `AGENTD_DESKTOP_PORT=6081` trong `.env`. Cổng này chỉ lắng nghe trên localhost của máy chạy Docker. Nếu Docker chạy trên server, có thể mở desktop qua SSH tunnel với `ssh -N -L 6080:127.0.0.1:6080 user@server`.
- Chromium dùng `--no-sandbox` để tương thích với cấu hình Docker mặc định. Container chạy bằng user thường và CDP chỉ mở trên loopback bên trong container.
- Dừng bằng `docker compose down`. Theo dõi desktop bằng `docker compose logs -f agentd`. Log ứng dụng nằm ở `/home/agentd/.agentd/data/agentd.log` trong container.

## Lệnh

`/status` cây agent + todo của worker · `/stop` dừng mọi worker · `/think [low|medium|high|xhigh|max|default]` mức suy nghĩ của chat · `/reload` nạp lại config/skills · `/memory` · `/forget <id>` · `/skills` · `/approve [id] [all]` `/deny [id]` duyệt lệnh · `/<tên-skill> <yêu cầu>`

Mức suy nghĩ của worker: chat tự chọn `effort` khi giao việc (hoặc theo lời dặn của bạn); mặc định từng model đặt ở `models.<tên>.effort`.
Agent tự chỉnh cấu hình của nó (skill `self-improve`, tool `self_read`/`self_write`/`self_reload`); mỗi lần ghi cần bạn duyệt và bản cũ được lưu ở `data/backup/`.

## Điều khiển máy tính & trình duyệt (role `operator`)

- **Trình duyệt thật** (`browser_*`): Chromium bình thường, có giao diện, profile cố định (`~/.agentd/browser`) nên giữ cookie/đăng nhập; user đăng nhập một lần qua VNC là agent dùng được. Không cờ automation, không bật `Runtime.enable`, script chạy trong isolated world, click/gõ bằng sự kiện trusted với chuyển động và nhịp như người. Bật bằng `browser.cdp` trong config + service chạy Chromium với `--remote-debugging-port` (chỉ localhost).
- **Desktop** (`cua_*`): [cua-driver](https://github.com/trycua/cua) qua MCP (`mcp_servers.cua`). MCP server bất kỳ cũng cắm được theo cách này; tool hiện ra dạng `<tên>_<tool>`.
- Operator không gõ mật khẩu/OTP, không giải CAPTCHA, và hỏi lại trước hành động không đảo ngược được (gửi, đăng, mua, xoá).
- Chat gửi file/ảnh cho user bằng `send_file` (Telegram: ảnh hiện trực tiếp, file khác dạng tài liệu).

## Cấu trúc

| Đường dẫn | Nội dung |
|---|---|
| `config.yaml` | provider, model (kèm "thẻ năng lực"), role, tool được phép, tool cần duyệt (`ask`), giới hạn |
| `souls/SOUL.md` | tính cách Chat Agent |
| `roles/*.md` | prompt nghề của từng worker |
| `skills/<tên>/SKILL.md` | skill: frontmatter `name/description/roles` + hướng dẫn + file đi kèm |
| `internal/agent` | runtime: cây agent, inbox/bus, delegate/spawn, report, giới hạn, duyệt |
| `internal/llm` | router + adapter Anthropic (SDK chính thức) và OpenAI-compatible |
| `internal/memory`, `internal/store` | log hội thoại, ghi nhớ, recall FTS5, chưng cất bằng model rẻ |
| `internal/tools` | web_search, web_fetch, file, shell, calc, get_time |
| `internal/gateway` | CLI, Telegram (nhận/gửi file, ảnh, album) |
| `internal/browser` | điều khiển Chromium qua CDP, kiểu người dùng thật |
| `internal/mcp` | MCP client (stdio) cho tool ngoài như cua-driver |

Dữ liệu ở `data/agentd.db`, log ở `data/agentd.log`, worker làm việc trong `workspace/`.

## Giới hạn hiện tại / việc tiếp theo

- Shell chỉ giới hạn thư mục + timeout + duyệt tay, **chưa phải sandbox thật**.
- Memory v2 (embedding, hồ sơ peer, `ask_memory`, dọn dẹp ban đêm), `tool_search` (nạp tool theo nhu cầu), skill tự học (skill-smith) — chưa làm.

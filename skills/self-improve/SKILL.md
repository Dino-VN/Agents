---
name: self-improve
description: Tự điều chỉnh bản thân — đổi tính cách, model, mức suy nghĩ (effort), quyền tool, role worker, tạo/sửa skill. Dùng khi user muốn agent thay đổi cách hoạt động, hoặc khi bạn thấy một quy trình/bài học lặp lại đáng thành skill.
roles: [chat, coder]
---
# Tự điều chỉnh bản thân

Cấu hình của bạn nằm trong thư mục home của agentd (xem bằng `self_read` với path rỗng). Đọc/ghi bằng `self_read` / `self_write`, áp dụng bằng `self_reload` — không cần khởi động lại.

| File | Điều khiển gì |
|---|---|
| `config.yaml` | model, effort mặc định, role worker, tool được phép, tool cần duyệt, giới hạn |
| `souls/SOUL.md` | tính cách, giọng nói, xưng hô của chat agent |
| `roles/<role>.md` | cách làm việc của từng loại worker |
| `skills/<tên>/SKILL.md` | skill: quy trình chuyên biệt, nạp khi cần |

Không đọc/ghi được `secrets.env` (API key, token) và `data/` — user tự đổi key bằng `agentd setup`.

## Quy trình bắt buộc
1. `self_read` file liên quan **trước** khi sửa. Sửa tối thiểu, giữ nguyên phần không liên quan (self_write ghi đè toàn bộ file).
2. Tóm tắt cho user thay đổi định sửa (cái gì → thành gì, vì sao) nếu thay đổi lớn hoặc user chưa nói rõ. Việc user đã yêu cầu cụ thể thì làm luôn.
3. `self_write` (bản cũ tự sao lưu vào data/backup/). `config.yaml` sai cú pháp/tham chiếu sẽ bị từ chối — đọc lỗi, sửa, ghi lại.
4. `self_reload`, rồi báo user đã đổi gì. Chat áp dụng từ tin nhắn tiếp theo; worker đang chạy giữ cấu hình cũ.

## Các thay đổi thường gặp

**Mức suy nghĩ (effort)** — `low | medium | high | xhigh | max`:
- Mặc định từng model: trường `effort:` dưới `models.<tên>` trong config.yaml.
- Chat của một cuộc trò chuyện: user gõ `/think <mức>` (không cần sửa file).
- Từng task: tham số `effort` khi delegate/spawn_agent.

**Đổi model cho một vai trò:** sửa `roles.<role>.model` (tên phải có trong `models:`). Thêm model mới: thêm mục dưới `models:` với `provider: main`, `id: <id model của API>`, `max_tokens`, `effort`, `card` (mô tả ngắn để chat biết khi nào chọn). Model mới chưa thử thì báo user có thể chưa chạy được.

**Quyền tool:** `roles.<role>.tools` (glob, vd `web_*`), `ask` = tool cần user duyệt, `shell_allow` = lệnh shell tự duyệt (glob, chỉ lệnh đơn không có ; | & $ > <). Chỉ nới quyền khi user yêu cầu rõ.

Tool hiện có: web_search, web_fetch, calc, get_time, read_file, write_file, edit_file, list_dir, grep, glob, shell, todo_write, delegate, spawn_agent, send_message, report, list_agents, agent_status, cancel_agent, recall, remember, forget, use_skill, read_skill_file, self_read, self_write, self_reload.

**Thêm role worker mới:** thêm mục dưới `roles:` (description, model, prompt: roles/<tên>.md, tools) và tạo file prompt tương ứng.

**Tính cách (SOUL.md):** chỉ sửa khi user **yêu cầu rõ** đổi tính cách/giọng/xưng hô. Giữ phần còn lại.

**Tạo skill mới:** `skills/<tên-gạch-nối>/SKILL.md` với frontmatter:
```
---
name: <tên>
description: <làm gì + KHI NÀO dùng — đây là thứ quyết định skill có được chọn hay không>
roles: [chat, coder]   # bỏ trống = mọi role
---
<các bước cụ thể, ngắn gọn, có ví dụ lệnh/đường dẫn nếu cần>
```
Tạo skill khi: user dạy một quy trình muốn lặp lại, hoặc cùng một bài học/cách làm xuất hiện nhiều lần. File tham khảo dài để trong `skills/<tên>/references/`.

## Không được
- Tự tăng quyền/giới hạn của chính mình khi user không yêu cầu.
- Xoá skill/role user đã tạo mà không hỏi.
- Đưa secrets vào bất kỳ file nào.

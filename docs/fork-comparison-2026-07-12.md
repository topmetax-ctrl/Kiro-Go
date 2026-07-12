# So sánh Kiro-Go với các fork upstream (2026-07-12)

So sánh project hiện tại với hai fork trên GitHub:
- `zsecducna/Kiro-Go`
- `ngh1105/Kiro-Go`

Cả hai đều fork từ cùng upstream `Quorinex/Kiro-Go` (giống gốc của repo này).
README của cả ba giống hệt nhau — khác biệt hoàn toàn nằm ở commit history.

## Kết luận tổng quát

Về tính năng lớn, project này **đi xa hơn cả hai fork**. Các tính năng dưới đây
không fork nào có:

- Web search (SearXNG + Tavily) như native `web_search` server-tool
- Upstream forwarding + realtime dashboard (per-provider/route metrics)
- Anti-detection (utls browser TLS fingerprint / TLS terminator / selective proxy)
- Inject account vào Kiro IDE/CLI local

Tuy nhiên, cả hai fork đã sửa một số bug / lỗ hổng bảo mật mà code hiện tại
**vẫn còn**. Đó mới là phần giá trị đáng lấy về.

## Bảng so sánh

| Nhóm tính năng | Local (repo này) | zsecducna | ngh1105 |
|---|---|---|---|
| Web search (SearXNG + Tavily) | Có (độc quyền) | Không | Không |
| Upstream forwarding + dashboard | Có (độc quyền) | Không | Không |
| Anti-detection (utls/TLS terminator) | Có (độc quyền) | Không | Không |
| Inject account vào Kiro IDE local | Có (độc quyền) | Không | Không |
| Multi-region probe | Có (chỉ external_idp) | Có (đầy đủ hơn) | Có (thêm idc) |
| Responses API — cô lập theo API key | **Thiếu** | Thiếu | Có |
| Refresh-token dedup (chống burn token Azure) | **Thiếu** | Thiếu | Có |
| Prompt cache persist + O(1) LRU + metrics | Chỉ in-memory | Chỉ in-memory | Có |
| Multi-region profile picker (Azure tenant) | Không | Có | Không |
| Import hardening (DoS cap, dup guard, allow-list) | Một phần | Có | Có |

## Những thứ đáng port về (xếp theo mức ưu tiên)

### 1. Cô lập Responses API theo API key (từ ngh1105) — LỖ HỔNG BẢO MẬT
- Vị trí: `proxy/responses_store.go:84` — `loadResponse(id)` không có tham số owner/key.
- Vấn đề: bất kỳ API key nào cũng có thể đọc stored response của key khác qua
  `previous_response_id` → rò rỉ hội thoại chéo người dùng.
- Fork sửa: thêm `loadResponseForOwner`, trả lỗi generic ("stored response not found")
  để không cho phép enumerate response ID.

### 2. Refresh-token dedup (từ ngh1105) — BUG THẬT với tài khoản Azure/Entra
- Vị trí: `proxy/handler.go:285` — refresh token không có mutex dedup.
- Vấn đề: với external_idp (Microsoft Entra), refresh token là one-time-use, xoay vòng.
  Hai request đồng thời refresh cùng account → đốt token đã xoay → account bị khoá.
- Fork sửa: gom mọi lần refresh qua `refreshAccountToken(account, force)` dưới một mutex,
  và đọc token mới nhất đã persist trước khi refresh để không tái dùng token đã xoay.

### 3. idc fallback region probe (từ ngh1105)
- Vị trí: `proxy/kiro_api.go:147,216` — chỉ probe fallback region cho `external_idp`.
- Vấn đề: tài khoản Enterprise SSO (`idc`) có portal ở us-east-1 nhưng profile ở
  eu-central-1 sẽ bị kẹt, không tìm ra profile.
- Fork sửa: thêm `idc` vào điều kiện probe fallback.

### 4. Import hardening (từ zsecducna)
- `http.MaxBytesReader(1MiB)` chặn DoS khi import credentials.
- Guard trùng account id trong `AddAccount`.
- Re-validate tokenEndpoint theo IdP allow-list.

### 5. Multi-region profile picker + refresh model cache sau khi đổi profile (từ zsecducna)
- Một credential Azure AD có thể chứa profile ở nhiều region (US + EU).
- Prober hiện dừng ở region đầu tiên → profile EU không với tới được.
- Fork thêm `DiscoverKiroProfiles` (probe mọi region), UI picker, và refresh model cache
  ngay sau khi đổi profile (model list scope theo region).

### 6. Prompt cache: persist to disk + O(1) LRU + metrics (từ ngh1105)
- Cache hiện chỉ in-memory (không có `prompt_cache.json`).
- Nâng cấp hiệu năng/observability, không phải bug.

## Ghi chú
Hai fork KHÔNG "giá trị hơn" về tính năng — repo này dẫn trước rõ rệt.
Giá trị lớn nhất cần lấy là 2 bản vá của ngh1105 (mục #1 và #2): cả hai là
bug/lỗ hổng thật trong code hiện tại.

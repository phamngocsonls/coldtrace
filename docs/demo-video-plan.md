# ColdTrace — Kế hoạch quay video demo

Kế hoạch sản xuất đầy đủ cho một **video demo sản phẩm 10 phút**: quay gì, theo
thứ tự nào, nói gì, và làm sao giấu những đoạn nhàm chán. Mọi mốc thời gian
dưới đây đều **đo trực tiếp** trên stack này (không phải ước lượng), và mỗi
cảnh đều có phương án dự phòng để một trục trặc nhỏ không thể làm hỏng buổi quay.

---

## 0. Câu chuyện một đoạn (thông điệp video phải truyền tải)

> 12 tủ đông chứa 250.000 USD hải sản. Một compressor chết lúc 02:00. Tủ vượt
> −14 °C lúc 02:40 và −10 °C — ngưỡng hư hỏng — lúc 03:20. Lúc 02:00 không ai
> đang trực.

> ColdTrace là "AI operator" luôn trực: nó phân loại sự cố từ dấu vết nhiệt
> trong **~3 phút**, giải thích **tại sao** (top factors + LLM root-cause),
> nhớ **các sự cố tương tự**, ra quyết định **trong khuôn khổ guardrail**
> (class đã duyệt, độ tin cậy, ETA, fleet circuit breaker), tự khắc phục
> (**lệnh → thiết bị ACK trong 0–38 ms**), rồi **tự đóng** incident khi tủ
> chứng minh được là đã khỏe lại — và toàn bộ chuỗi là một trace trong Datadog.

> Và khi nó **không thể** tự sửa — không có hành động từ xa, hoặc thiết bị từ
> chối — nó nói rõ và **gọi người trực**, với thông tin soạn sẵn. Ba con đường
> xử lý (tự động / gọi on-call / escalate), một cửa audit duy nhất.

**Cung "khắc phục" là xương sống của video.** Mọi sự cố kết thúc bằng một
trong ba hành động, và cả ba đều được trình diễn: **A** tự restart, **B** điều
người xử lý, **C** ngắt quyền tự động (breaker), cộng **A′** — AI thất bại một
cách an toàn.

---

## 1. Thiết lập quay

| Hạng mục | Đề xuất |
|---|---|
| Công cụ | OBS Studio (hoặc Screen Studio để tự zoom) |
| Cửa sổ | Trình duyệt **1512×982**, Chrome trên macOS, zoom 100% |
| Vùng quay | Console `http://localhost:3301` + một cửa sổ terminal để chạy `curl` inject |
| Con trỏ | Bật "click highlight" (OBS: hiệu ứng Cursor Spotlight) |
| Âm thanh | Thu lời thuyết minh **sau** khi quay hình, theo từng cảnh — dễ quay lại hơn |
| Giao diện | Dùng theme tối của console như khi quay — đó chính là "bộ mặt" sản phẩm |
| Tốc độ | 60 fps nếu có lia giữa các trang; 30 fps vẫn chấp nhận được |

**Bố cục hai terminal rất hiệu quả:** trái = console, phải = terminal chạy lệnh
`curl` inject, để người xem **thấy** hành động → phản ứng.

---

## 2. Checklist trước khi quay (T-30 phút)

**Nắm sản phẩm trước (nếu chưa):** `docs/QUICKSTART.md` §1–§6 — pitch 60
giây, bảng tour console, cheat sheet demo và 5 khái niệm chính. Trang đó cộng
với kế hoạch này là đủ để lên hình; `solution-design.md` là tài liệu tham
chiếu sâu nếu giám khảo hỏi xoáy.

Chạy theo thứ tự; mỗi dòng đều được `scripts/demo_preflight.sh` kiểm chứng.

```bash
docker compose up -d --wait                                # stack khỏe
MONGO_URI=mongodb://localhost:27017 go run ./tools/initdb   # có index
MONGO_URI=mongodb://localhost:27017 go run ./tools/seedhistory
#   → 24 incident đã resolve (bộ nhớ AI) + 6 ngày bucket giờ + 21 ngày bucket ngày
#   + một excursion thô 22 phút trên FZ-009 → CSV HACCP có dữ liệu ngay ngày đầu
./scripts/demo_cleanup.sh                                  # 12×OK, không có incident OPEN
./scripts/demo_preflight.sh                                # phải đạt 12/12
```

Sau đó là **quy tắc thời gian sống còn** (lý do số 1 khiến buổi quay hỏng):

> **Chờ ~7 phút sau cleanup trước lần inject đầu tiên.** Classifier cần một
> cửa sổ ấm ~6 phút (72 mẫu @ 5 s) trước khi đủ tự tin. Khi đã ấm, phát hiện
> rơi vào **~2,5–4 phút sau inject**; khi nguội là **~6,5 phút**. Không chạy
> cleanup, không restart `ai-agent`, không gọi `/api/demo/reset` trong 7 phút
> cuối trước một cảnh.

Trạng thái cần xác nhận trước khi quay:

- `GET /api/approvals` → **chỉ `compressor_failure` được duyệt** (video cần
  `power_outage` ở trạng thái PROPOSED trước, rồi mới trình diễn khoảnh khắc
  duyệt)
- `GET /api/policy` → `minConfidence 0.85`, `maxEtaAlarmMinutes 45`,
  `fleetCircuitBreaker 2`, kill switch tắt
- Console mở ở Fleet Overview, SSE chạy live (tile đổi mỗi 5 s)
- Datadog đã đăng nhập trên `app.ap1.datadoghq.com` (traces + logs)

---

## 3. Kịch bản (từng cảnh)

Tổng thời lượng mục tiêu: **10:00** (mười một cảnh). Thời gian chờ phát hiện
thật được giấu bằng **cắt** (xem §5), nên thời gian quay thực tế vẫn khoảng
~40 phút cho video 10 phút. Xương sống là **khắc phục**: ba con đường hành
động (tự động / on-call / escalate) cộng A′ (AI thất bại an toàn), đều được
trình diễn.

### Cảnh 1 — Vấn đề (0:00–0:40) *không cần stack chạy*

- **Chiếu:** `docs/screenshots` hoặc màn mở đầu của deck: tủ ấm dần −18 →
  −14 → −10 trong khi không ai trực. Một con số trên màn hình: *"FZ-010 chứa
  25.000 USD vaccine — ngưỡng hư hỏng là −10 °C."*
- **Nói:** thông điệp ở §0. Kết bằng: *"Đây là ColdTrace. Để tôi giết một
  compressor và xem nó xử lý thế nào."*
- Vì sao mở đầu thế: giám khảo quyết định trong 30 giây đầu liệu phần còn lại
  có đáng xem không. Một tủ đông chết là toàn bộ business case.

### Cảnh 2 — Tổng quan fleet + thang trạng thái (0:40–1:30)

- **Chiếu:** console `/` — 12 tile xanh, dải KPI, rollup theo Site, feed sự
  kiện chạy live (SSE, không polling).
- **Nói:** *"Mười hai tủ đông, ba site, tất cả xanh. Trạng thái được TÍNH
  chứ không lưu — STALE → ALARM → AT_RISK → OK, có deadband để compressor
  đang nghỉ không nhuộm vàng cả fleet. Chạy live qua MongoDB change streams —
  không refresh, không polling."*
- **Khoảnh khắc chứng minh:** chỉ vào một tile có nhiệt độ cập nhật mỗi 5 s.

### Cảnh 3 — MongoDB bên dưới (1:30–2:20) ⭐ cảnh nhà tài trợ

- **Chiếu:** trang chi tiết thiết bị → **biểu đồ History**, rồi bật công tắc
  tầng **hourly → daily** và để biểu đồ vẽ lại (7 ngày bucket giờ, rồi 3 tuần
  bucket ngày). Có thể chia đôi màn hình với `docs/mongodb-data-model.md` §1
  (sơ đồ phân quyền sở hữu dữ liệu).
- **Nói (câu chuyện Mongo trong 50 giây):** *"Mỗi mẫu đọc rơi vào một
  time-series collection của MongoDB — metaField giữ nhãn thiết bị nên bộ lọc
  theo site bỏ qua nguyên bucket. Mẫu thô hết hạn sau 24 giờ, nhưng rollup
  `$merge` giữ hourly 7 ngày và daily một năm — biểu đồ bạn vừa thấy đổi tầng
  là đang đọc ROLLUP, bucket theo giờ địa phương, không phải dữ liệu thô.
  Lưới live là change streams: một tail oplog, không polling. Và registry được
  validate bằng `$jsonSchema` — một thiết bị sai định dạng không thể ghi vào
  được."*
- Đây là cảnh trả lời "vì sao MongoDB" — time series + phân tầng + change
  streams + schema validation trong một cú máy liên tục.

### Cảnh 4 — Giết một tủ đông: phát hiện (2:20–4:00) ⭐ cảnh chính

- **Làm:** terminal: `POST /api/demo/inject {"deviceId":"FZ-006","fault":"power_outage"}`
  — hoặc dùng ngăn Demo trong console (cùng tác dụng, trực quan hơn).
- **Trong lúc chờ (~90 s trên hình):** thuyết minh — *"classifier đang nhìn
  cửa sổ trượt 6 phút; 13 đặc trưng; nó cần ~3 phút vật lý sự cố trước khi
  dám kết luận."* Có thể cho thấy lưới fleet chuyển AT_RISK trên FZ-6.
- **CẮT** (xem §5) → quay lại khi incident OPEN. **Chiếu:** trang Incidents:
  card mới.
  - `faultClass: power_outage`, `confidence: 1.00`
  - `decision: PROPOSED` — *"sự cố đầu tiên thuộc class này — chờ người duyệt"*
    ← **nói to câu này: AI KHÔNG hành động khi chưa được phép**
  - **topFactors dạng thanh**: `power_mean`, `compressor_duty` — *"đây là
    những yếu tố dẫn tới quyết định; model trình bày cách nó nghĩ"*
  - Tile FZ-006 giờ **AT_RISK/ALARM** trên lưới fleet.
- **Nói:** *"~3 phút sau sự cố, classifier gọi đúng là mất điện với độ tin cậy
  tuyệt đối — nhưng đây là lần đầu gặp, nên ColdTrace đề xuất chứ không hành
  động. Con người nằm trong vòng lặp theo thiết kế."*

### Cảnh 5 — Giải thích: LLM root-cause + trí nhớ (4:00–5:00) ⭐ cảnh AI

Hai panel trên trang incident, theo thứ tự:

1. **Explain → RCA**: LLM (engine OneNexus/OpenAI-compatible) viết phân tích
   nguyên nhân gốc **dựa trên số liệu thật của cửa sổ** — tóm tắt, nguyên
   nhân, các bằng chứng, hành động đề xuất. Nói: *"Một LLM chỉ nhìn được
   telemetry thật của thiết bị này — nó không thể bịa ra một con số."*
   (Nếu LLM chết: engine template tất định trả lời thay — cùng định dạng, vẫn
   dựa trên dữ liệu. Demo không bao giờ gãy.)
2. **Similar past incidents**: 3 kết quả, cosine trên vector 13 chiều,
   *"99,6% — cùng dấu hiệu hỏng với FZ-012 tuần trước"*. Nói: *"Agent có trí
   nhớ. Nó đã nhận ra pattern này."*
3. **Link trace**: bấm *"Open trace in Datadog"* — chuyển tới Cảnh 8 hoặc mở
   tab mới để dùng sau.

### Cảnh 6 — Duyệt → TỰ ĐỘNG khắc phục → TỰ ĐỘNG đóng (5:00–6:40) ⭐⭐ cao trào

Đây là cảnh quyết định — vòng lặp khép kín trọn vẹn, và phải quay như MỘT
chuỗi liên tục:

- **Làm:** trang AI Control → duyệt `power_outage`… hoặc hay hơn cho nhịp
  video: duyệt `compressor_failure` (đã duyệt sẵn) và inject
  `POST /api/demo/inject {"deviceId":"FZ-002","fault":"compressor_failure"}`.
- **Chờ ~2,5–4 phút** → **CẮT** → quay lại ở `decision: AUTO`.
- **Chiếu & nói, theo thứ tự:**
  1. Card: `decision: AUTO`, confidence ~0,97, ETA ~35 phút — *"class đã duyệt,
     các cổng độ tin cậy và ETA đều đạt trong khuôn khổ guardrail"*
  2. **Commands/Events:** `compressor_restart` **QUEUED → SENT → ACKED**,
     độ trễ **~12 ms** — *"agent khắc phục qua đúng đường MQTT mà con người
     vẫn dùng, với audit trail đầy đủ"*
  3. Tile FZ-002 hồi phục trên lưới (nhiệt độ kéo xuống)
  4. **TỰ ĐỘNG ĐÓNG** (phần ai cũng bỏ quên): chờ thêm ~2–3 phút → **CẮT** →
     card chuyển **RESOLVED, `resolvedBy: system:recovery`** — *"không ai bấm
     gì cả. Sau 60 giây telemetry chứng minh khỏe mạnh, agent tự đóng incident
     của chính nó. Phát hiện → hành động → PHỤC HỒI → học."*
- Đoạn kết (đừng bỏ — đây là nửa "học" của vòng lặp):
  `POST /api/incidents/{id}/feedback {"resolvedClass":"compressor_failure"}`
  → `"learned": true`, log agent `ai_learned trainSamples: 801` — *"và nó vừa
  học từ chính episode này."*

### Cảnh 7 — Guardrail: fleet circuit breaker (6:40–7:50) ⭐ cảnh lòng tin

- **Làm:** inject `compressor_failure` trên **hai thiết bị cùng lúc**
  (`FZ-004` + `FZ-005`).
- **Chờ ~4 phút** → **CẮT** → cả hai incident OPEN:
  - FZ-004: `AUTO` — đã khắc phục
  - FZ-005: **`HOLD` — "fleet circuit breaker: quá nhiều thiết bị trong ALARM —
    tạm dừng hành động tự động, cần người review"**
- **Nói:** *"Hai compressor chết cùng lúc nhiều khả năng là sự cố lưới điện
  hoặc một lô hàng lỗi — chứ không phải mười hai hành động AI độc lập. Breaker
  tạm dừng quyền tự chủ và gọi người. Và nó LATCHED: khi FZ-004 hồi phục,
  FZ-005 KHÔNG âm thầm chuyển sang AUTO — trạng thái hold giữ nguyên cho tới
  khi episode được xử lý."*
  (LATCHED-HOLD này được một regression test giữ chặt — nó từng gây lỗi thật
  trong lúc rehearsal.)
- **Nhịp on-call (~20 s, phần khắc phục của incident bị hold):** khi FZ-005
  đang hold, con người hoàn tất — **ACK** incident → **điều kỹ thuật viên**
  (console Remediate; event `technician_dispatched`, `issuedBy: sre-console`)
  → thiết bị được sửa tại chỗ, và sau 60 s chứng minh khỏe mạnh agent
  **tự đóng** (`system:recovery`) — việc điều phối của operator nằm trong
  audit trail. **Nói:** *"Breaker không chỉ chặn AI — nó trao việc cho con
  người. Một cú ack, một lần điều phối, cùng một audit trail. Đó là toàn bộ
  câu chuyện khắc phục: ba con đường — tự động, on-call, escalate — cùng một
  cửa."*
- Nhắc các guardrail khác khi đang ở trang AI Control:
  `minConfidence 0.85` (chính cổng quyết định incident có MỞ hay không),
  `maxEtaAlarmMinutes 45`, cooldown 30 phút mỗi thiết bị, kill switch.

### Cảnh 8 — Datadog: dashboard → chuỗi trace (7:50–8:45)

> **Tình trạng dashboard (áp dụng lại ngày 2026-11-08 với PAT hoạt động):**
> dashboard *"ColdTrace — Cold-chain Fleet Health"* + 3 monitor đang **LIVE**
> trong org — `./datadog/apply-datadog.sh` đã đẩy lên khi `.env` có
> `DD_APP_KEY` (personal access token) hợp lệ. Chạy lại script đó sau mỗi lần
> sửa `datadog/dashboard.json`; metrics/logs/traces đã được xác minh chạy với
> 0 lỗi export.

- **Chiếu (mở đầu):** dashboard — `app.ap1.datadoghq.com/dashboard/v9y-45k-jsz`
  — 10 widget: nhiệt độ theo thiết bị với mốc −10/−14 °C, bộ đếm AT_RISK/ALARM
  từ custom query MongoDB của Agent, ingest latency + batch size, quyết định AI
  theo phân loại, log stream compressor-fail. Nói: *"đúng câu chuyện mà một
  phòng NOC sẽ treo lên tường — mỗi widget là chính pipeline bạn vừa xem, trong
  telemetry production."*
- **Chiếu:** từ card incident → *"Open trace in Datadog"* → APM trace:
  `mqtt.ingest → mongo.write → ai.decision`, mỗi quyết định một span, kèm
  thuộc tính `ai.top_factor.*`.
- **Chiếu:** cùng `traceId` trong Log Explorer → log liên kết với trace. Nói:
  *"một định danh theo mẫu đọc từ broker tới database tới quyết định AI — đây
  là chuỗi audit mà thanh tra an toàn thực phẩm sẽ đòi hỏi."*

### Cảnh 9 — Khép vòng tuân thủ + sức khỏe model (8:45–9:15)

- **Chiếu:** trang Compliance → export CSV HACCP:
  `FZ-009, 19.1 min trên −14 °C, xấu nhất −11.5, 2026-11-08 06:34 (giờ ICT)`
  — *"số phút trên ngưỡng tính từ delta mẫu thật, excursion cuối cùng theo múi
  giờ của thanh tra — đây là tài liệu đưa vào hồ sơ HACCP."*
- **20 s:** card Model Health (100% accuracy, 1.100 cửa sổ mới, confusion
  matrix sạch) — *"chấm trên cửa sổ render từ đúng vật lý simulator mà fleet
  đang chạy — đo, không lưu sẵn."*
- **10 s:** trang Events — toàn bộ audit trail bạn vừa tạo trên hình.

### Cảnh 10 — Tóm tắt ba con đường khắc phục + AI biết dừng đúng lúc (9:15–9:45) ⭐ lòng tin

Montage nhanh (30 s), ba card trên màn hình — ba hành động, mỗi cái một câu:

| Con đường | Câu |
|---|---|
| **A — TỰ ĐỘNG** | *"Tự restart trong 38 ms — vì bạn đã duyệt class đó và mọi cổng đều đạt."* |
| **B — ON-CALL** | *"Không sửa được? Gọi người trực kèm cách sửa viết sẵn: một cú ack, một lần điều phối."* |
| **C — ESCALATE** | *"Hai cái cùng lúc? Nó dừng lại, và nhờ người xem xét cả fleet."* |

Rồi nhịp A′, từ card incident (hoặc để dành trả lời Q&A nếu bạn cắt bớt):
hiện rõ `escalationNote` — *"restart THẤT BẠI 2 lần — quá tải nhiệt latched —
hành động thủ công: reboot"* → **Nói:** *"AI thử hai lần, rồi làm đúng việc
một SRE giỏi: dừng lại, gọi người, và viết xuống mọi thứ họ cần. Escalate
không phải là hệ thống hỏng — đó là hệ thống đang chạy đúng."*

### Cảnh 11 — Kết (9:45–10:00)

- **Nói:** *"Phát hiện trong vài phút, tự giải thích, hành động trong khuôn
  khổ guardrail, chứng minh đã hồi phục, và biết khi nào phải bàn giao. Đó là
  ColdTrace."* + repo/QR nếu muốn.

---

## 4. AI thực sự làm gì (bản đồ năng lực — dùng làm B-roll/lời thuyết minh)

| # | Năng lực | Xuất hiện ở đâu |
|---|---|---|
| 1 | **Phân loại** 5 class sự cố (softmax trên 13 đặc trưng, warm-start từ vật lý simulator) | Cảnh 3 — conf 1.00 |
| 2 | **Cổng độ tin cậy** — incident chỉ MỞ khi vượt `minConfidence` của operator (0.85), cùng cổng với quyền tự chủ | Cảnh 3/6 — "không nhiễu, không có bóng ma 0,58" |
| 3 | **Dự báo ETA** tới ngưỡng −10 °C; HOLD nếu vượt chân trời (45 phút) | Cảnh 5 — "ETA 35 phút, trong guardrail" |
| 4 | **Giải thích**: topFactors (weight × z mỗi đặc trưng) vẽ dạng thanh | Cảnh 3 |
| 5 | **Nguyên nhân gốc** bằng LLM dựa trên số liệu cửa sổ thật (fallback template nếu API chết) | Cảnh 4 |
| 6 | **Ghi nhớ**: cosine similarity trên vector sự cố cũ → "similar incidents" | Cảnh 4 |
| 7 | **Quyết định trong guardrail**: duyệt class, độ tin cậy, ETA, cooldown, fleet circuit breaker, kill switch | Cảnh 5–6 |
| 8 | **Hành động**: MQTT `compressor_restart` với command lifecycle đầy đủ (QUEUED→SENT→ACKED, 0–38 ms) | Cảnh 5 |
| 9 | **Chứng minh hồi phục**: chuỗi 60 s khỏe mạnh → tự đóng `system:recovery` | Cảnh 5 ⭐ |
| 10 | **Học**: feedback của operator → `Learn()` huấn luyện lại, `trainSamples: 801` | Cảnh 5 đoạn kết |
| 11 | **Tracing toàn chuỗi**: một `traceId` broker → DB → quyết định → Datadog | Cảnh 7 |
| 12 | **Tự xử lý thất bại của chính mình** ⭐: restart bị từ chối (thiết bị ack `ok:false`) → retry sau backoff → escalate lên on-call với trang thông tin soạn sẵn — không lặp vô hạn, không ngồi im | Cảnh 7 B-roll / Q&A |

### Dấu vết sự cố — vì sao "tốc độ đổi nhiệt" CHÍNH LÀ bộ phân loại

Mỗi class sự cố có tốc độ leo nhiệt đặc trưng (`internal/sim/device.go`), và
các đặc trưng mạnh nhất của model đều là đặc trưng tốc độ (`trend_lag_cpm`,
`net_trend_cpm`, `slope_cpm` — trọng số cao nhất trong mọi quyết định thật):

| Class sự cố | Dấu vết tốc độ nhiệt | Tín hiệu phân biệt dứt điểm | Sửa từ xa được? |
|---|---|---|---|
| `compressor_failure` | leo **+0.3 °C/phút** | compressor dừng, vẫn có công suất điện | ✅ `compressor_restart` |
| `door_left_open` | leo **+0.1 °C/phút** | `doorOpen: true`, compressor chạy hết công suất | ❌ nhân viên đóng cửa |
| `sensor_drift` | **−0.25 °C/phút** theo *số đọc* (cảm biến báo thấp hơn thực) | compressor bình thường, các tủ cạnh bên lạnh | ❌ thay đầu dò |
| `power_outage` | leo ấm ≈ compressor chết | **công suất điện bằng 0** — mọi thứ chết | ❌ khôi phục điện tại chỗ |
| healthy | dao động ±~1.4 °C (răng cưa thermostat) | không leo liên tục, cửa sổ đầy đủ | — |

**Câu thuyết minh:** *"Mỗi sự cố có một dấu vân tay ở tốc độ — và mức độ trung
thực — của nhiệt độ. Chỉ riêng tốc độ tách được ba class; hai class còn lại
cần tín hiệu chéo, và `etaAlarmMin` chỉ là `(ngưỡng − nhiệt) / tốc độ` — model
làm phép chia để nói cho bạn biết còn kịp hành động."*

### Sổ tay khắc phục — ba con đường hành động bạn trình diễn

Xuyên suốt video là KHẮC PHỤC: có thứ gì đó hỏng → một trong ba hành động sửa
nó. Mọi con đường đều để lại cùng một audit trail (commands + events), và đó
chính là điểm mấu chốt: **AI và con người hành động qua cùng một cửa.**

| Con đường | Khi nào | Quyết định | Hành động (quay cái này) | Bằng chứng audit trên hình |
|---|---|---|---|---|
| **A — TỰ ĐỘNG** *(restart)* | class đã duyệt + conf ≥ 0.85 + ETA ≤ 45' + breaker sạch | `AUTO` | AI tự gửi `compressor_restart` → thiết bị ACKED ~12 ms → nhiệt kéo xuống → chuỗi 60 s khỏe | `QUEUED→SENT→ACKED`, `remediatedBy: ai-agent`, `resolvedBy: system:recovery` |
| **B — ON-CALL** *(điều người)* | class chưa duyệt, hoặc lần đầu gặp, hoặc ETA > 45' | `HOLD` / `PROPOSED` | Operator **ACK** → **điều kỹ thuật viên** (console Remediate → `technician_dispatched`) → sửa tại chỗ → hồi phục | `acknowledged: true`, event `technician_dispatched`, lệnh `issuedBy: sre-console`, resolve + `feedback` |
| **C — ESCALATE** *(breaker)* | ≥2 thiết bị hỏng cùng lúc | `HOLD` (giữ suốt episode) | Tạm dừng tự chủ → người review cả fleet → xử lý từng thiết bị | `circuitBreaker: true`, "repeat actions blocked by cooldown" |

Lệnh chính xác cho con đường B (con đường cần người):

```bash
INCIDENT_ID=<lấy từ card incident>
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/ack -H 'X-Operator: on-call'  # nhận xử lý (không body)
curl -X POST localhost:8099/devices/FZ-005/remediate                               # điều kỹ thuật viên (không body)
# …thiết bị được sửa tại chỗ, nhiệt hồi phục. Sau 60 s chứng minh khỏe mạnh agent
#   TỰ ĐÓNG (resolvedBy system:recovery) — hoặc tự đóng tay, không body:
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/resolve -H 'X-Operator: on-call'
curl -X POST localhost:8092/api/incidents/$INCIDENT_ID/feedback \
     -H 'Content-Type: application/json' -d '{"resolvedClass":"door_left_open"}'   # → learned: true
```

**Nói:** *"Khi AI làm được, nó làm — và đưa hoá đơn. Khi không làm được, có
người trực: một cú ack, một lần điều phối, cùng một audit trail. Ba con đường,
một cửa, không có gì xảy ra ngoài sổ sách."*

### Con đường A′ — khi chính TỰ ĐỘNG thất bại (cảnh agentic-failure)

Restart là một lệnh, và lệnh có thể **bị thiết bị từ chối** — quá tải nhiệt
latched (`compressor_failure_hard`) ack `ok:false` và sự cố VẪN CÒN. Agent
không lặp vô hạn và không ngồi chờ "awaiting recovery":

1. restart lần 1 → **BỊ TỪ CHỐI** (lệnh `FAILED`, lý do trong `ackDetail`)
2. chờ 30 s → **retry lần 2** → lại BỊ TỪ CHỐI
3. → **ESCALATE lên on-call**: incident nhận `escalated: true` +
   `escalationNote` (class, độ tin cậy, ETA, lý do từ chối, hành động thủ
   công, episode tương tự gần nhất) và audit log nhận trang
   `incident_escalated` mức **critical**. Con người power-cycle controller
   (`reboot`) — đúng hành động mà note chỉ ra.

```bash
curl -X POST localhost:8092/api/demo/inject -H 'Content-Type: application/json' \
     -d '{"deviceId":"FZ-005","fault":"compressor_failure_hard"}'      # sự cố chống lại ta
# …phát hiện → AUTO → restart BỊ TỪ CHỐI → retry → BỊ TỪ CHỐI → ESCALATE (~1 phút sau phát hiện)
curl -X POST localhost:8092/api/devices/FZ-005/commands -H 'Content-Type: application/json' \
     -H 'X-Operator: on-call' -d '{"action":"reboot"}'                 # cách sửa của người, đúng như trang thông tin ghi
```

**Nói:** *"AI thử hai lần, rồi làm đúng việc một SRE giỏi: dừng lại, gọi người,
và viết xuống mọi thứ họ cần — class, độ tin cậy, còn bao lâu tới hư hỏng,
thiết bị nói gì, và cách sửa. Escalate không phải hệ thống thất bại; đó là hệ
thống đang chạy đúng."*

---

## 5. Dựng phim: giấu thời gian chờ

Phát hiện là vật lý thật — **~2,5–4 phút** từ inject tới incident, và tự đóng
cần thêm **~2–3 phút**. Đừng bao giờ để người xem ngồi nhìn.

| Mẹo | Cách làm |
|---|---|
| **Cắt cứng + overlay đồng hồ** | Cắt từ inject → incident. Chèn chữ "T+3:10" (text OBS hoặc sticker CapCut) để cú cắt trung thực |
| **Tăng tốc** | Time-lapse 8–12× lưới fleet trong lúc tile ấm lên — vừa thấy vật lý vừa giữ nhịp |
| **Thuyết minh đè lên cú cắt** | Giải thích thang trạng thái / guardrail trong lúc chạy time-lapse |
| **Cảnh 6 là cảnh duy nhất dài** | Chuỗi AUTO→ACKED→hồi phục→RESOLVED cần ~5 phút thật; quay một mạch với ramp 10×, rồi cắt gọn |

---

## 6. Phương án dự phòng (để buổi quay không thể chết)

| Nếu cái này hỏng | Làm thế này |
|---|---|
| LLM RCA chưa sẵn sàng | Engine template trả lời cùng định dạng — thuyết minh đó là fallback được thiết kế ("keyless mode") |
| Phát hiện > 5 phút | Cửa sổ chưa ấm: `POST /api/demo/reset` → chờ đủ 7 phút → inject lại. Không bao giờ inject lại mà không reset |
| Phân loại sai class | Giữ nguyên! Chiếu `ai_class_flip` + dedup anchored-episode ("một episode mỗi thiết bị, anchor đứng yên") — đó là tính năng |
| Incident không mở (conf < 0.85) | Đó là cổng đang chạy đúng; inject thiết bị khác hoặc thuyết minh về cổng độ tin cậy |
| Datadog chậm | Trace lưu trên doc incident (`traceId`) — mở sau khi quay, ghép clip vào |
| Stack hỏng | `scripts/demo_preflight.sh` in ra cách sửa chính xác cho từng mục — cứ quay cảnh chạy nó nếu cần (đó là vận hành trung thực) |

---

## 7. Thứ tự quay — trình tự quay ≠ thứ tự cảnh (một buổi ~40 phút)

Thời gian chờ phát hiện và chờ cửa sổ ấm khiến quay theo thứ tự cảnh tốn hàng
giờ. Hãy quay theo trình tự này, rồi cắt thành thứ tự cảnh:

| Bước | Quay (đồng hồ thực) | Thành cảnh |
|---|---|---|
| 1 (T+0) | `demo_cleanup.sh` → `demo_preflight.sh` trên hình (12/12 xanh) — rồi **bật đồng hồ 7 phút warm-up và quay thuyết minh Cảnh 1 + Cảnh 2** | 1, 2 |
| 2 (T+7) | Inject `power_outage` FZ-006 → **trong lúc chờ, quay Cảnh 3** (đổi tầng History + câu chuyện Mongo — không cần sự cố) **và phần walkthrough guardrail ở AI-Control** | 3, 7 (nửa guardrail) |
| 3 (T+11) | FZ-006 OPEN → quay card incident, topFactors, **Explain → RCA**, similar-incidents, lý do PROPOSED | 4, 5 |
| 4 (T+15) | Inject `compressor_failure` FZ-002 (class đã duyệt) → quay thuyết minh chuyển trạng thái AT_RISK/ALARM trên lưới | 2 (chuyển cảnh), 6 (chuẩn bị) |
| 5 (T+19) | Card AUTO xuất hiện → quay decision/gates → **Commands: QUEUED→SENT→ACKED ~12 ms** → tile hồi phục | 6 |
| 6 (T+24) | **Quay liền mạch qua lúc tự đóng** (2–3 phút — ramp tốc độ sau) → `system:recovery` lật card → feedback → `learned: true` | 6 (khép vòng lặp) |
| 7 (T+27) | Inject FZ-004 **và** FZ-005 → quay walkthrough Events/Compliance trong lúc chờ | 9 |
| 8 (T+31) | Cả hai OPEN → quay FZ-004 AUTO vs FZ-005 **HOLD latched** (+ một nhịp cho thấy FZ-005 vẫn HOLD sau khi FZ-004 hồi phục) → rồi **khép bằng on-call**: ACK FZ-005 → điều kỹ thuật viên (console Remediate) → hồi phục → `system:recovery` | 7 |
| 9 (T+35) | Từ card incident → **Open trace in Datadog** → chuỗi span + log tương quan theo trace_id | 8 |
| 10 (T+38) | Trang Compliance → export CSV (dòng FZ-009 seed sẵn) → Model Health (100%, 1.100 cửa sổ) → thuyết minh kết | 9, 10 |
| 11 (T+40) | `demo_cleanup.sh` + `demo_preflight.sh` lần nữa nếu cần quay bù — **nhưng chờ lại 7 phút trước mọi lần inject mới** | quay bù |

Vì sao thứ tự này hiệu quả: mọi lần inject đều khi cửa sổ đã ấm; mọi khoảng
chờ đều được lấp bằng cảnh không cần sự cố trực tiếp; phần tự đóng (chờ lâu
nhất) quay cuối để nếu hỏng cũng không kéo đổ các cảnh trước.

---

## 8. Sổ số liệu: những con số được phép trích dẫn (đo trong buổi rehearsal này)

- Phát hiện: **148–255 s** khi cửa sổ ấm, **387 s** khi cửa sổ nguội
  (hôm nay FZ-002/011: 243–255 s sau khi sửa)
- Độ tin cậy khi phát hiện: **0.90–1.00** (FZ-002 0.908 / ETA 44.8 phút —
  sát cổng; FZ-011 0.995 / ETA 34.8)
- Độ trễ ACK lệnh: **0–12 ms** (xác minh lại sau khi sửa lỗi reconnect của
  ops-api — restart broker cưỡng bức không còn làm lệnh rot thành TIMEOUT;
  `set_fault` ACKED 0 ms xuyên qua lần reset)
- Tự đóng: **~91–373 s** sau khi khắc phục (`system:recovery`)
- Xử lý thất bại của agent: restart cứng **bị từ chối → retry sau 30 s
  backoff → bị từ chối → escalate** trong chưa tới ~90 s từ lúc phát hiện,
  kèm `escalationNote` soạn sẵn + event `incident_escalated` mức critical —
  rồi on-call chạy `reboot` (đúng như note chỉ ra) để đóng lại
- Circuit breaker: FZ-004 AUTO + FZ-005 **HOLD latched** — đã xác minh nó
  KHÔNG lật khi FZ-004 hồi phục
- Con đường on-call (B): `incident_ack` → `technician_dispatched` → sửa thiết
  bị → `system:recovery` — toàn bộ chuỗi audit khắc phục bởi con người, đã
  xác minh đầu-cuối
- Model eval: **100%**, 1.100 cửa sổ simulator mới, 5 class
- CSV Compliance: 19.1 phút excursion, xấu nhất −11.5 °C, cột giờ địa phương
- Preflight: **12/12 kiểm tra xanh** trước mọi buổi quay

# MongoDB: WiredTiger, index, `explain`, transaction

MongoDB lưu **document** (BSON) trong collection, không schema cố định. Hiệu năng quy về: *index có khớp truy vấn không* (đọc bao nhiêu key/document so với trả về), *document có vừa mô hình truy cập không* (nhúng hay tham chiếu), và *write/read concern* bạn chọn.

Chuẩn bị: `go run . mongo` trong [`seed/`](seed/) (xem [README](README.md#dữ-liệu-mẫu)) tạo database `playground` với `play_customers` (10k) và `play_orders` (300k đơn, mỗi đơn có `items` là mảng nhúng). Transaction cần replica set (kể cả một node: `--replSet rs0` rồi `rs.initiate()`). Gắn connection tradar vào `mongodb://.../playground`; các block dưới dùng alias `mongo-local` — đổi thành connection của bạn. Ví dụ đã chạy trên MongoDB 8.3.

Giới hạn của tradar (xem trang wiki `Mongo-Redis-Elasticsearch` trong repo tradar): parser tối giản, **không có `createIndex()`, `startSession`/transaction, bulkWrite**. Tạo index đi qua `db.runCommand({createIndexes: ...})`; thí nghiệm transaction (§4) phải dùng `mongosh`. `explain()` chỉ có ở cuối chain của `find`.

## 1. Lưu trữ: WiredTiger

- Engine mặc định là **WiredTiger**: document lưu trong **B-tree** theo khoá nội bộ (`RecordId`); `_id` có index riêng trỏ tới đó. Mỗi collection và mỗi index là một cấu trúc B-tree riêng.
- **MVCC + snapshot**: mọi đọc thấy một snapshot nhất quán (khác Postgres ở chỗ phiên bản cũ nằm trong *history store*/cache của WiredTiger, không lẫn vào dữ liệu chính); ghi dùng **khoá mức document** — hai ghi vào hai document khác nhau không chờ nhau.
- **Cache** WiredTiger (mặc định ~50% RAM trừ 1 GB) giữ trang đã giải nén; phần còn lại dựa vào cache file hệ điều hành (dữ liệu trên đĩa **nén**, mặc định `snappy`). Working set (dữ liệu + index hay dùng) không vừa cache thì mọi truy vấn thành đọc đĩa — nguyên nhân chậm số một.
- **Journal** (WAL) + **checkpoint** (mặc định 60 s): ghi vào journal trước, định kỳ ghi snapshot nhất quán xuống file dữ liệu; crash thì khôi phục từ checkpoint rồi replay journal.
- **Oplog** (`local.oplog.rs`, capped collection): với replica set, mọi ghi được ghi thành entry idempotent; secondary đọc oplog để áp lại. Cửa sổ oplog (vài giờ–vài ngày) quyết định secondary tụt lại bao lâu thì phải resync hoàn toàn.
- Giới hạn document **16 MB**; mảng nhúng vô hạn lớn dần là anti-pattern. Document bị ghi lớn hơn → ghi lại cả document (không có "update tại chỗ" như nghĩ).

Kích thước:

```js mongo-local
db.runCommand({collStats: "play_orders"})
```

Đọc `size` (dữ liệu chưa nén ~51 MB trong lần thử), `storageSize` (trên đĩa sau nén ~13 MB), `indexSizes`, `avgObjSize` (~171 B). Index tính vào working set: 3 index phụ ở §2 cộng thêm ~11 MB, hơn 20% so với dữ liệu.

```js mongo-local
db.getCollectionInfos()
```

## 2. Index và `explain`

Index mặc định là **B-tree** trên một hoặc nhiều field, có thứ tự; trỏ tới document. Loại khác: **multikey** (index trên field mảng — một entry mỗi phần tử; tự động), **text**, **2dsphere/2d** (địa lý), **hashed**, **wildcard**, **TTL** (xoá tự động theo thời gian), **partial/sparse**, **unique**.

Cách đọc plan: `find(...).explain()` (tradar chạy ở chế độ `executionStats`):

| Field | Ý nghĩa |
| --- | --- |
| `winningPlan.stage` | `COLLSCAN` (quét cả collection), `IXSCAN` (quét index), `FETCH` (đọc document), `SORT` (sắp trong RAM), `PROJECTION_COVERED` (trả thẳng từ index) |
| `nReturned` | số document trả về |
| `totalKeysExamined` | số entry index đã quét |
| `totalDocsExamined` | số document đã đọc |
| `executionTimeMillis` | thời gian |

**Tỉ lệ cần nhìn:** `totalDocsExamined` (và `totalKeysExamined`) so với `nReturned`. Lý tưởng gần 1:1. `COLLSCAN` với `totalDocsExamined` = cả collection là dấu hiệu thiếu index.

Chưa có index nào ngoài `_id`:

```js mongo-local
db.play_orders.find({customerId: 42}).explain()
```

Lần thử: `COLLSCAN`, `nReturned 36`, `totalDocsExamined 300000`, ~215 ms. Tạo index:

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [{key: {customerId: 1}, name: "customerId_1"}]})
```

Chạy lại `explain` → `FETCH > IXSCAN`, `totalKeysExamined 36`, `totalDocsExamined 36`, ~2 ms. **Covered query** (chỉ lấy field nằm trong index, và loại `_id`) không đọc document nào:

```js mongo-local
db.play_orders.find({customerId: 42}, {_id: 0, customerId: 1}).explain()
```

→ `PROJECTION_COVERED > IXSCAN`, `totalDocsExamined 0`.

### Index nhiều field và quy tắc ESR

Thứ tự field trong index **quan trọng**. Gợi ý **ESR**: **E**quality (so `=`) → **S**ort → **R**ange (`$gt`, `$lt`, `$in` lớn…). Mục tiêu là index trả document đã đúng thứ tự `sort`, khỏi sắp trong RAM (stage `SORT` bị giới hạn 100 MB mặc định, vượt thì lỗi hoặc cần `allowDiskUse`).

Truy vấn "đơn `paid` 7 ngày qua, sắp theo `amount` giảm dần":

```js mongo-local
db.play_orders.find({status: "paid", createdAt: {$gt: ISODate("2026-10-25T00:00:00Z")}}).sort({amount: -1}).explain()
```

Tạo hai index để so sánh (`{status, createdAt}` kiểu E-R, và `{status, amount: -1, createdAt}` kiểu E-S-R):

```js mongo-local
db.runCommand({createIndexes: "play_orders", indexes: [
  {key: {status: 1, createdAt: 1}, name: "status_createdAt"},
  {key: {status: 1, amount: -1, createdAt: 1}, name: "status_amount_createdAt"}
]})
```

Rồi dùng `.hint(<tên hoặc key>)` ép từng index và so `totalKeysExamined`:

```js mongo-local
db.play_orders.find({status: "paid", createdAt: {$gt: ISODate("2026-10-25T00:00:00Z")}}).sort({amount: -1}).hint({status: 1, amount: -1, createdAt: 1}).explain()
```

Kết quả lần thử cho thấy **ESR chỉ là kim chỉ nam, không phải luật**:

| Điều kiện | Index | Stage | `totalKeysExamined` | ms |
| --- | --- | --- | --- | --- |
| 7 ngày (rất chọn lọc, 1436 doc), không `limit` | `{status, createdAt}` | `SORT > FETCH > IXSCAN` | 1436 | ~8 |
| như trên | `{status, amount, createdAt}` (ESR) | `FETCH > IXSCAN` (không sort) | **39 831** | ~157 |
| 300 ngày, `limit(20)` | `{status, createdAt}` | `SORT > FETCH > IXSCAN` | 61 554 | ~225 |
| như trên | `{status, amount, createdAt}` (ESR) | `LIMIT > FETCH > IXSCAN` | **26** | ~0 |

Khi range chọn lọc, quét theo thứ tự `amount` phải đi qua nhiều key không thoả range rồi mới lọc — đắt hơn lấy ít document rồi sắp. Khi range rộng và có `limit` nhỏ, ESR cho đúng 20 document đầu mà khỏi sắp. Planner không hint cũng chọn đúng ở trường hợp thứ hai (`wide, auto`). Bài học: **luôn đo bằng `explain`, thay vì thuộc quy tắc**.

Xem index nào thật sự được dùng và tốn bao nhiêu:

```js mongo-local
db.play_orders.aggregate([{$indexStats: {}}])
```

(`accesses.ops` bằng 0 lâu ngày → ứng viên bỏ; mỗi index làm chậm ghi và tốn RAM/đĩa.) Lưu ý: `$indexStats` đếm từ lần khởi động server.

### Multikey, partial, TTL

- Index trên field mảng (`items.sku`) tạo một entry **mỗi phần tử**; `find({"items.sku": "sku-7"})` dùng `IXSCAN` bình thường (lần thử: 1221 key, 1221 doc). Index nhiều field chỉ được có **tối đa một** field mảng.
- **Partial index** (`partialFilterExpression`) chỉ index document thoả điều kiện — nhỏ, nhanh khi truy vấn luôn kèm điều kiện đó.
- **TTL** (`expireAfterSeconds` trên field ngày) xoá document hết hạn nền (~60 s/lần), có sẵn trong cheatsheet của tradar.
- Truy vấn không dùng được index: `$where`, regex không neo đầu (`/abc/`), `$ne`/`$nin` trên tập lớn, so sánh trên giá trị tính toán.

## 3. Aggregation và mô hình dữ liệu

- `aggregate` là pipeline các stage. Đặt `$match` và `$sort` **sớm** để dùng index; từ sau `$group`/`$project` thì index vô dụng. Stage `$group` giữ dữ liệu trong RAM (giới hạn 100 MB/stage, `allowDiskUse` cho tràn đĩa).
- `$lookup` là join: thường chậm trên tập lớn; field `foreignField` **phải có index**. Trong lần thử, `$lookup` từ `play_orders` sang `play_customers` theo `_id` dùng index mặc định nên nhanh.

```js mongo-local
db.play_orders.aggregate([
  {$match: {customerId: 42}},
  {$lookup: {from: "play_customers", localField: "customerId", foreignField: "_id", as: "customer"}},
  {$limit: 3}
])
```

- Nhúng hay tham chiếu: **nhúng** dữ liệu luôn được đọc cùng nhau (đơn + `items`) → một lần đọc, ghi nguyên tử trên một document; **tham chiếu** khi dữ liệu dùng chung, thay đổi riêng, hoặc phát triển không giới hạn (comment của bài viết). Mongo không có join rẻ như quan hệ — thiết kế theo truy vấn.
- Phân trang: `skip(n)` quét và bỏ `n` document → dùng range trên field có index (`_id`, `createdAt`) thay `skip` lớn.

## 4. Transaction, concern

### Nguyên tử mức document

Mọi thao tác trên **một document** là nguyên tử, kể cả khi sửa nhiều field và mảng nhúng. Đây là lý do mô hình nhúng giảm nhu cầu transaction. `$inc`, `$set`, `findAndModify` nguyên tử không cần transaction (lần thử: `$inc` hai lần cộng đúng).

### Multi-document transaction

Từ 4.0 (replica set), 4.2 (sharded): `startTransaction` … `commitTransaction` qua **session**. Dùng **snapshot isolation**: mọi đọc trong transaction thấy snapshot lúc bắt đầu; ghi của người khác không hiện cho tới khi commit và ghi của bạn không hiện ra ngoài trước commit (lần thử: ghi trong transaction chưa commit, đọc ngoài vẫn thấy giá trị cũ).

Chạy bằng `mongosh` (tradar chưa hỗ trợ):

```js
const session = db.getMongo().startSession();
const orders = session.getDatabase("playground").play_orders;
session.startTransaction({readConcern: {level: "snapshot"}, writeConcern: {w: "majority"}});
orders.updateOne({_id: someId}, {$set: {amount: 1}});
// ... phiên khác đọc _id này: vẫn thấy giá trị cũ ...
session.abortTransaction();   // hoặc commitTransaction()
```

**Xung đột ghi**: hai transaction cùng sửa một document — không chờ như khoá hàng của Postgres, mà transaction thứ hai **thất bại ngay** với `WriteConflict` và nhãn `TransientTransactionError`:

```js
const s1 = db.getMongo().startSession(), s2 = db.getMongo().startSession();
s1.startTransaction(); s2.startTransaction();
s1.getDatabase("playground").play_orders.updateOne({_id: id}, {$set: {amount: 1}});
s2.getDatabase("playground").play_orders.updateOne({_id: id}, {$set: {amount: 2}});   // WriteConflict
```

Lần thử: lỗi `WriteConflict`, `errorLabels: ["TransientTransactionError"]`. Mã ứng dụng phải **retry cả transaction** khi gặp nhãn đó (driver có `withTransaction` làm sẵn).

Chi phí: transaction giữ snapshot/tài nguyên, mặc định **tự huỷ sau 60 s** (`transactionLifetimeLimitSeconds`), nên giữ ngắn và ít thao tác. Nếu hầu hết thao tác của bạn cần transaction, có thể mô hình dữ liệu chưa hợp Mongo.

### Read concern, write concern

| Cài đặt | Ý nghĩa | Đánh đổi |
| --- | --- | --- |
| `w: 1` | primary xác nhận | nhanh; ghi có thể **rollback** nếu primary sập trước khi nhân bản |
| `w: "majority"` | đa số node đã ghi | bền qua failover; chậm hơn (mặc định từ 5.0) |
| `j: true` | đã vào journal | bền khi crash một node |
| `readConcern: "local"` | dữ liệu của node này | có thể đọc dữ liệu sau này bị rollback |
| `readConcern: "majority"` | dữ liệu đã được đa số xác nhận | không bị rollback, có thể hơi cũ |
| `readConcern: "snapshot"` | snapshot nhất quán (trong transaction) | dùng cho transaction |
| `readPreference: secondary` | đọc từ secondary | giảm tải primary; có thể đọc dữ liệu cũ (replication lag) |

Hệ quả: `w: 1` + failover là cách mất dữ liệu đã "thành công"; `readPreference: secondary` không đảm bảo thấy ghi vừa làm trừ khi dùng *causal consistency* (session).

## 5. Checklist tối ưu một truy vấn chậm

1. Tìm câu chậm: profiler (`db.setProfilingLevel(1, {slowms: 100})` rồi `show profile` trong tradar) hoặc `currentOp` (có trong cheatsheet), log chậm của server.
2. `find(...).explain()`: so `totalDocsExamined`/`totalKeysExamined` với `nReturned`; có `COLLSCAN`/`SORT` trong RAM không.
3. Thêm hoặc đổi index theo (E, S, R) rồi **đo lại** — đừng tin quy tắc (bảng §2).
4. Chỉ lấy field cần (projection); thử covered query nếu truy vấn nóng.
5. `skip` lớn → range. `$or` → mỗi nhánh phải có index. Regex → neo đầu chuỗi.
6. Aggregation: `$match`/`$sort` trước, `$project` sớm để giảm kích thước document đi qua pipeline.
7. Kiểm working set: `indexSizes` + dữ liệu hay đụng so với cache WiredTiger (`db.serverStatus().wiredTiger.cache`).
8. Document quá lớn hoặc mảng nhúng phình → đổi mô hình.
9. Ghi nhiều: `insertMany` với `ordered: false`; tránh nhiều index thừa.

## 6. Bài tập

1. Tìm index tốt nhất cho "đơn của khách X, mới nhất trước, 20 dòng". Chứng minh bằng `explain` rằng stage `SORT` biến mất và `totalKeysExamined` ≈ 20.
2. Tái hiện bảng ESR ở §2 với các khoảng thời gian khác nhau (1 ngày, 30 ngày, 300 ngày) có và không có `limit`. Tại sao kết quả lật ngược?
3. Dùng `partialFilterExpression: {status: "new"}` cho một index; so kích thước với index đầy đủ và kiểm tra truy vấn nào dùng được.
4. Làm sai có chủ ý: truy vấn `{status: {$ne: "cancelled"}}` — plan thế nào? Vì sao `$ne` kém?
5. Dựng hai transaction xung đột (§4) và viết vòng retry đúng cho `TransientTransactionError`.
6. Với `w: 1` vs `w: "majority"`: giải thích bằng sơ đồ thời gian kịch bản mất dữ liệu khi primary sập; thử bằng `docker stop` một node trong replica set 3 node.

## Tài liệu tham khảo

Tài liệu chính thức MongoDB: *Indexes*, *Query Plans* và *Analyze Query Performance* (`explain`), *Equality, Sort, Range*, *Transactions*, *Read Concern / Write Concern*, *WiredTiger Storage Engine*, *Replication* (oplog). Sách/khoá: MongoDB University *M201 MongoDB Performance*.

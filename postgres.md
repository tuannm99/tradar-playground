# Postgres: lưu trữ, index, planner, transaction

Chuẩn bị: chạy `go run . postgres` trong [`seed/`](seed/) (xem [README](README.md#dữ-liệu-mẫu)). Tạo `play_customers` (10k dòng) và `play_orders` (500k dòng, chỉ có khoá chính). Các block dưới dùng alias `pg-local` — đổi thành connection của bạn. Ví dụ đã chạy trên Postgres 16.

## 1. Lưu trữ: heap, tuple, MVCC

- Bảng là **heap** các page 8 KB; mỗi dòng là một **tuple** nằm trong page, địa chỉ là `ctid = (page, slot)`. Tuple mang header với `xmin` (transaction tạo) và `xmax` (transaction xoá/khoá, `0` nếu còn sống).
- **`UPDATE` không sửa tại chỗ**: nó ghi một tuple mới (xmin mới) và đặt `xmax` trên tuple cũ. `DELETE` chỉ đặt `xmax`. Tuple cũ ("dead") vẫn nằm trong heap cho tới khi **vacuum** dọn. Đây là gốc của bloat, và của việc `UPDATE` một cột cũng tốn như ghi cả dòng.
- **Đọc** = mỗi transaction (hoặc statement) có một *snapshot*; tuple nhìn thấy được nếu `xmin` đã commit trong snapshot và `xmax` chưa. Không ai chặn ai: reader không khoá writer.
- Giá trị lớn (> ~2 KB) được nén/chuyển sang **TOAST** (bảng phụ); `select *` trên bảng nhiều cột to đọc cả TOAST nếu bạn lấy cột đó.

```sql pg-local
select ctid, xmin, xmax, id from play_orders limit 3;
```

Quan sát MVCC bằng mắt (cột `ctid` đổi sau `UPDATE`, `xmin` thành transaction mới):

```sql pg-local
select ctid, xmin, xmax, id, status from play_orders where id = 1;
update play_orders set status = status where id = 1;
select ctid, xmin, xmax, id, status from play_orders where id = 1;
```

Kích thước và tuple chết:

```sql pg-local
select relname, relpages, reltuples::bigint, pg_size_pretty(pg_relation_size(oid)) as heap
from pg_class where relname like 'play_%';
```

```sql pg-local
select relname, n_live_tup, n_dead_tup, last_autovacuum, last_autoanalyze
from pg_stat_user_tables where relname like 'play_%';
```

### Vacuum, WAL

- **VACUUM** đánh dấu chỗ của tuple chết là tái dùng được (không trả đĩa cho OS, trừ `VACUUM FULL` — khoá bảng, viết lại cả bảng), cập nhật **visibility map** (page nào toàn tuple nhìn thấy được — `Index Only Scan` cần nó, xem §3) và **đóng băng** xid cũ. Autovacuum chạy nền; tuple chết không dọn được khi có **transaction mở lâu** (snapshot cũ còn cần chúng) — nguyên nhân bloat phổ biến nhất.
- xid là số 32 bit, quay vòng: không vacuum/freeze kịp thì Postgres tự dừng nhận ghi để tránh *wraparound*. Theo dõi `age(datfrozenxid)`.
- **WAL**: mọi thay đổi ghi vào log (tuần tự, `fsync` lúc commit) *trước* khi trang heap/index được ghi; crash thì replay WAL. Commit nhanh vì chỉ cần WAL xuống đĩa, không phải trang dữ liệu. `synchronous_commit = off` đổi độ bền (mất vài trăm ms cuối khi crash) lấy tốc độ, **không** gây hỏng dữ liệu.
- **HOT update**: nếu `UPDATE` không đổi cột nào có index và page còn chỗ, tuple mới nằm cùng page, không cần thêm entry vào index → rẻ hơn nhiều. `fillfactor < 100` chừa chỗ cho việc này.

```sql pg-local
select relname, n_tup_upd, n_tup_hot_upd from pg_stat_user_tables where relname like 'play_%';
```

## 2. Index

Mặc định là **B-tree**: giữ khoá có thứ tự → hỗ trợ `=`, `<`, `>`, `BETWEEN`, `ORDER BY`, `LIKE 'abc%'` (với collation `C` hoặc `text_pattern_ops`). Index trỏ tới `ctid`, nên đọc dòng đầy đủ luôn phải ra heap (trừ index-only, §3).

Loại khác: **Hash** (chỉ `=`), **GIN** (nhiều giá trị trong một dòng: `jsonb`, mảng, full-text), **GiST** (hình học, range), **BRIN** (bảng rất lớn có thứ tự vật lý tương quan với cột, như `created_at` append-only: index cực nhỏ).

Quy tắc cần nhớ:

- **Index nhiều cột `(a, b)`** dùng được cho `a`, `a AND b`, không dùng tốt cho `b` đơn lẻ. Thứ tự: cột lọc `=` trước, cột `range`/`ORDER BY` sau.
- **Partial index** `... WHERE status = 'new'` nhỏ và nhanh nếu query luôn kèm điều kiện đó.
- **Covering** `INCLUDE (col)` thêm cột vào lá index để có index-only scan mà không làm nó thành khoá.
- Hàm trên cột (`where lower(email) = ...`) không dùng được index thường → **expression index**.
- Mỗi index làm chậm `INSERT`/`UPDATE` (và chặn HOT) và tốn đĩa. Đừng tạo "phòng xa".
- `CREATE INDEX CONCURRENTLY` không khoá ghi nhưng chậm hơn và không chạy trong transaction.

```sql pg-local
select indexrelid::regclass as index, idx_scan, pg_size_pretty(pg_relation_size(indexrelid)) as size
from pg_stat_user_indexes where relname like 'play_%';
```

(`idx_scan = 0` lâu ngày = ứng viên bỏ.)

## 3. Planner và `EXPLAIN`

Planner ước lượng **chi phí** của từng cách (cost, đơn vị tương đối) dựa trên **thống kê** (`ANALYZE` thu vào `pg_stats`) rồi chọn rẻ nhất. Sai plan gần như luôn do **ước lượng số dòng sai** (thống kê cũ, cột tương quan, hàm trên cột).

Đọc plan: từ **trong ra ngoài, dưới lên**; mỗi node có `cost=khởi động..tổng`, `rows` (ước lượng) và với `ANALYZE` thêm `actual time`, `rows` (thật), `loops` (số lần node chạy — **nhân** thời gian và số dòng mỗi lần với `loops`).

```sql pg-local
explain (analyze, buffers) select * from play_orders where customer_id = 42;
```

Chưa có index → `Parallel Seq Scan`: đọc cả ~3.9k page (`Buffers: shared hit=3933`), bỏ 500k − 48 dòng (`Rows Removed by Filter`). Thêm index rồi chạy lại:

```sql pg-local
create index play_orders_customer_idx on play_orders (customer_id);
explain (analyze, buffers) select * from play_orders where customer_id = 42;
```

→ `Bitmap Index Scan` + `Bitmap Heap Scan`: ~50 page thay vì ~3.9k, từ chục ms xuống dưới 1 ms. (Bitmap chứ không phải `Index Scan` thường vì 48 dòng nằm rải rác khắp heap; bitmap gom theo page rồi đọc heap theo thứ tự.)

**Index-only scan cần visibility map.** Ngay sau khi nạp dữ liệu, truy vấn chỉ cần cột trong index vẫn ra heap (page chưa được đánh dấu all-visible):

```sql pg-local
explain (analyze, buffers) select customer_id from play_orders where customer_id = 42;
vacuum play_orders;
explain (analyze, buffers) select customer_id from play_orders where customer_id = 42;
```

Lần đầu vẫn `Bitmap Heap Scan`; sau `VACUUM` thành `Index Only Scan` với `Heap Fetches: 0`. Bảng bị ghi liên tục thì `Heap Fetches` tăng và lợi thế mất dần.

### Các node hay gặp

| Node | Khi nào | Dấu hiệu xấu |
| --- | --- | --- |
| `Seq Scan` | đọc cả bảng | `Rows Removed by Filter` rất lớn so với dòng trả về |
| `Index Scan` / `Index Only Scan` | ít dòng, có thứ tự | `Heap Fetches` cao (index-only) |
| `Bitmap Index/Heap Scan` | vài trăm–vài nghìn dòng rải rác | `Heap Blocks: lossy` (thiếu `work_mem`) |
| `Nested Loop` | bên ngoài ít dòng, bên trong có index | `loops` rất lớn với bên trong là Seq Scan |
| `Hash Join` | join bằng `=`, không cần index | `Batches > 1` (hash tràn ra đĩa) |
| `Merge Join` | hai bên đã sắp theo khoá | thêm `Sort` đắt |
| `Sort` | `ORDER BY`, join, group | `Sort Method: external merge Disk` |
| `HashAggregate` / `GroupAggregate` | `GROUP BY` | `Batches > 1` |

```sql pg-local
explain (analyze, buffers)
select c.country, sum(o.amount) from play_orders o join play_customers c on c.id = o.customer_id group by c.country;
```

Quan sát: `Hash Join` (xây hash trên bảng nhỏ `play_customers`, quét bảng lớn một lần), `Partial`/`Finalize` aggregate nhờ parallel. Đây là query phải đọc *hết* orders — index không giúp được; không ngạc nhiên nếu planner vẫn chọn `Seq Scan`.

### Ước lượng và thống kê

```sql pg-local
select attname, n_distinct, correlation, most_common_vals from pg_stats where tablename = 'play_orders';
```

- `n_distinct` âm = tỉ lệ so với số dòng (`-1` = mọi giá trị khác nhau).
- `correlation` gần ±1 = thứ tự vật lý khớp thứ tự cột → index scan rẻ, **BRIN** dùng được. Gần 0 (như `customer_id` ở đây) = các dòng của một khoá rải khắp heap.
- Hai cột lệch nhau (`country` ↔ `city`) làm planner nhân độc lập và đánh giá thấp → `CREATE STATISTICS ... (dependencies)`.

Điều kiện mà index thường không giúp, và vì sao thử cho thấy:

```sql pg-local
explain (analyze, buffers)
select * from play_orders where status = 'paid' and created_at > now() - interval '7 days';
```

Chỉ có index trên `customer_id`, nên vẫn `Seq Scan`. Thử `create index on play_orders (status, created_at);` rồi chạy lại: so sánh `Buffers` và chọn `Bitmap` hay `Index Scan`. Sau đó thử `status` đơn lẻ (4 giá trị, mỗi giá trị 25% bảng): planner thường **bỏ qua** index vì đọc 25% bảng qua index đắt hơn quét tuần tự — index kém chọn lọc thì vô ích.

### Phải đo, đừng đoán

- `EXPLAIN` thường chỉ ước lượng; `EXPLAIN ANALYZE` **thực sự chạy** câu lệnh. Với `UPDATE`/`DELETE`/`INSERT` nó ghi thật — bọc `BEGIN; ... ROLLBACK;`.
- So sánh `rows` ước lượng và thật ở từng node: lệch cả bậc độ lớn là chỗ cần xem trước tiên.
- Chạy 2–3 lần: lần đầu `shared read` (đĩa), lần sau `shared hit` (cache). Đừng so một lần nguội với một lần ấm.
- Theo dõi toàn hệ thống thay vì từng câu: extension `pg_stat_statements` (`shared_preload_libraries`, rồi `create extension pg_stat_statements`):

```sql pg-local
select calls, round(total_exec_time::numeric, 1) as total_ms, round(mean_exec_time::numeric, 2) as mean_ms, rows, left(query, 80) as query
from pg_stat_statements order by total_exec_time desc limit 10;
```

## 4. Transaction, isolation, lock

**ACID** trong Postgres: *Atomicity* + *Durability* nhờ WAL; *Isolation* nhờ MVCC + lock; *Consistency* nhờ constraint. Mỗi statement ngoài `BEGIN` là một transaction riêng (autocommit).

### Mức cô lập

| Mức | Snapshot lấy | Chặn được | Còn lại |
| --- | --- | --- | --- |
| `READ COMMITTED` (mặc định) | **mỗi statement** | dirty read | non-repeatable read, phantom, lost update, write skew |
| `REPEATABLE READ` | **một lần**, ở statement đầu | + non-repeatable, phantom | write skew; đụng dòng đã bị sửa → lỗi `could not serialize access` |
| `SERIALIZABLE` (SSI) | như RR + theo dõi phụ thuộc | tất cả anomaly | có thể abort bất kỳ transaction nào (`40001`) → **code phải retry** |

Postgres không có `READ UNCOMMITTED` thật (chạy như `READ COMMITTED`: không bao giờ có dirty read).

### Thí nghiệm hai phiên

Cần **hai phiên song song** giữ transaction mở. Trong tradar, `in_transaction` được theo dõi trên connection, nhưng việc `BEGIN` ở một lần `rr` rồi `UPDATE` ở lần `rr` sau có dùng cùng một session Postgres hay không **chưa được kiểm chứng** ở đây — thử trước; nếu mỗi `rr` lấy connection khác từ pool thì dùng `psql` hai cửa sổ (`docker exec -it <container> psql -U postgres`) cho các thí nghiệm này.

**A. Non-repeatable read** (`READ COMMITTED` thấy giá trị mới, `REPEATABLE READ` thì không):

```sql
-- Phiên 1                                   -- Phiên 2
begin;
select amount from play_orders where id=1;
                                             update play_orders set amount = amount + 1000 where id = 1;
select amount from play_orders where id=1;  -- RC: thấy +1000; RR: vẫn giá trị cũ
commit;
```

Chạy lại với `begin isolation level repeatable read;`.

**B. Lost update** — hai phiên cùng đọc rồi ghi `amount = <đọc> + 1` ở `READ COMMITTED` mất một lần cộng. Cách tránh, theo độ ưu tiên: (1) viết thẳng `set amount = amount + 1` (atomic, phiên sau chờ khoá dòng rồi đọc lại giá trị mới); (2) `select ... for update` rồi mới tính; (3) `REPEATABLE READ`/`SERIALIZABLE` + retry khi gặp `40001`.

**C. Khoá dòng:**

```sql
-- Phiên 1
begin;
select * from play_orders where id = 1 for update;
-- Phiên 2
update play_orders set status = 'paid' where id = 1;   -- đứng chờ tới khi phiên 1 commit/rollback
select * from play_orders where id = 2 for update skip locked;  -- bỏ qua dòng đang bị khoá: mẫu "hàng đợi công việc"
```

Xem ai đang chờ ai:

```sql pg-local
select pid, state, wait_event_type, wait_event, pg_blocking_pids(pid) as blocked_by, left(query, 60) as query
from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid();
```

**D. Deadlock:** phiên 1 `update ... where id = 1` rồi `... id = 2`; phiên 2 làm ngược thứ tự (`id = 2` rồi `id = 1`). Sau `deadlock_timeout` (1 s) Postgres huỷ một phiên với `40P01`. Phòng: luôn khoá theo cùng thứ tự (ví dụ `order by id` rồi `for update`).

**E. Write skew** (chỉ `SERIALIZABLE` chặn): hai bác sĩ cùng đang trực, mỗi người kiểm "còn ≥ 2 người trực" rồi tự nghỉ → còn 0 người. Hai transaction ghi dòng *khác nhau* nên không đụng nhau ở RR. Tự dựng bảng nhỏ để thử.

### Điểm hay dính

- **Transaction mở lâu** (`idle in transaction`): giữ snapshot → vacuum không dọn được, giữ khoá. Đặt `idle_in_transaction_session_timeout`.
- `ALTER TABLE` cần khoá mạnh; nếu nó phải chờ một transaction dài thì **mọi query sau nó xếp hàng sau nó**. Dùng `lock_timeout` khi chạy migration.
- Sequence (`bigserial`) không rollback → id có lỗ; đừng dùng làm số liên tục.

## 5. Checklist tối ưu một query chậm

1. Lấy câu thật và tham số thật (`pg_stat_statements` cho biết câu nào tốn tổng thời gian nhiều nhất, không chỉ chậm nhất).
2. `explain (analyze, buffers)`. Tìm node tốn nhất theo *actual time × loops* và so `rows` ước lượng/thật.
3. Ước lượng lệch → `analyze <bảng>`; cột tương quan → `create statistics`; tăng `default_statistics_target` cho cột lệch phân bố.
4. `Seq Scan` bỏ phần lớn dòng → index khớp điều kiện (đúng thứ tự cột, partial nếu hợp). Điều kiện kém chọn lọc → đừng mong index cứu.
5. Index sẵn có mà không dùng: hàm/ép kiểu trên cột, kiểu dữ liệu lệch (`int` vs `text`), `like '%x'`, `OR` giữa các cột khác nhau.
6. `Sort`/`Hash` tràn đĩa → tăng `work_mem` *cho phiên* (`set work_mem = '64MB'`), đừng tăng toàn cục bừa (nhân với số node × số kết nối).
7. Phân trang bằng `offset` lớn quét và bỏ cả `offset` dòng → keyset (`where (created_at, id) < (...) order by created_at desc, id desc limit n`).
8. `select *` kéo cột to/TOAST không cần; `count(*)` chính xác trên bảng lớn luôn quét (dùng ước lượng `reltuples` nếu chấp nhận).
9. Đo lại cùng điều kiện (nguội/ấm) và ghi kết quả trước/sau.

## 6. Bài tập

1. Với `play_orders`, tìm index tối thiểu cho: "đơn `paid` của khách X trong 30 ngày gần nhất, mới nhất trước, 20 dòng". Giải thích vì sao thứ tự cột `(customer_id, status, created_at)` quan trọng và khi nào planner không cần `Sort`.
2. `update play_orders set amount = amount where id <= 100000;` rồi xem `n_dead_tup`, kích thước heap trước/sau `vacuum`, và `vacuum full`. Vì sao kích thước không giảm sau `vacuum` thường?
3. Làm hỏng ước lượng: `update play_orders set status = 'paid';` rồi **không** `analyze`, so `rows` ước lượng/thật cho `where status = 'new'`; sửa bằng `analyze`.
4. Dựng lại thí nghiệm lost update (B) và viết ba cách sửa; cho mỗi cách ghi ra mức isolation cần và có cần retry không.
5. Tạo một deadlock (D) và đọc log lỗi: Postgres báo gì về hai phiên và hai khoá?
6. Bật `pg_stat_statements`, chạy lại các bài trên, tìm câu tốn nhiều thời gian nhất theo `total_exec_time`.

## Tài liệu tham khảo

Tài liệu chính thức Postgres: chương *Concurrency Control* (MVCC, isolation, locking), *Performance Tips* (`EXPLAIN`, thống kê), *Indexes*, *Routine Vacuuming*, *Reliability and the Write-Ahead Log*. Sách *The Internals of PostgreSQL* (Hironobu Suzuki, miễn phí online) cho phần heap/MVCC/WAL.

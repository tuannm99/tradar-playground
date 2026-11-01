# ClickHouse: MergeTree, index thưa, tối ưu query, "transaction"

ClickHouse là cột (columnar) và **OLAP**: đọc/aggregate hàng triệu dòng trên vài cột, ghi theo lô lớn. Mọi quy tắc tối ưu đều quy về một câu: *đọc ít granule nhất có thể, trên ít cột nhất có thể*.

Chuẩn bị: chạy `go run . clickhouse` trong [`seed/`](seed/) (xem [README](README.md#dữ-liệu-mẫu)). Tạo `play_events` (3 triệu dòng, `order by (user_id, ts)`) và `play_events_by_ts` (cùng dữ liệu, `order by (ts)`). Block dưới dùng alias `ch-local` — đổi thành connection của bạn. Ví dụ đã chạy trên ClickHouse bản `latest` (cuối 2026); `EXPLAIN` có thể khác chi tiết giữa các phiên bản.

Lưu ý với tradar: connector ClickHouse nói chuyện qua HTTP (`FORMAT JSON`), mỗi câu độc lập — không có phiên giữ `BEGIN`, và cũng không cần (xem §4).

## 1. Lưu trữ: MergeTree

- Một bảng `MergeTree` là tập **part** bất biến. Mỗi `INSERT` (một block) tạo một part mới; **không bao giờ sửa part tại chỗ**. Nền, **merge** gộp các part nhỏ thành part lớn (cùng partition) — tên "MergeTree".
- Trong part, dữ liệu **sắp theo `ORDER BY`** và mỗi **cột là một file riêng** (nén riêng). Đọc 2 cột trong 50 cột chỉ đọc 2 file.
- **Partition** (`PARTITION BY`) tách part theo khoá (thường tháng/ngày) để bỏ cả partition nhanh (`DROP PARTITION`, TTL) và cắt bớt khi đọc. Không phải công cụ tăng tốc chính; quá nhiều partition (vd. theo `user_id`) là lỗi kinh điển.
- Bảng nhiều part nhỏ đọc chậm và tốn: ghi **theo lô** (hàng nghìn–trăm nghìn dòng/lần), đừng `INSERT` từng dòng.

Part của bảng sau khi nạp (nhiều part nhỏ, chưa merge):

```sql ch-local
select partition, count() as parts, sum(rows) as rows, sum(marks) as marks,
       formatReadableSize(sum(bytes_on_disk)) as on_disk
from system.parts where table = 'play_events' and active group by partition order by partition;
```

Ép merge để quan sát (chỉ để học — production để nền tự merge):

```sql ch-local
optimize table play_events final;
```

Chạy lại câu trên: mỗi partition còn một part (trong lần thử: 21 part → 7 part, 1 part/tháng).

Nén theo cột: cột `payload` (chuỗi lặp 100 ký tự) 308 MiB thô còn ~11 MiB; `country`/`event` dùng `LowCardinality` còn ~1.6 MiB mỗi cột.

```sql ch-local
select name, type, formatReadableSize(data_compressed_bytes) as compressed,
       formatReadableSize(data_uncompressed_bytes) as raw
from system.columns where table = 'play_events' order by data_compressed_bytes desc;
```

Chọn kiểu nhỏ nhất đủ dùng (`UInt32` thay `Int64`, `LowCardinality(String)` cho cột ít giá trị, `DateTime` thay chuỗi) — cột nhỏ hơn = ít byte đọc hơn.

## 2. Index thưa (sparse primary index)

- Khác Postgres: khoá chính **không** là ràng buộc duy nhất và không trỏ tới từng dòng. Dữ liệu chia thành **granule** (mặc định 8192 dòng); index chỉ lưu giá trị `ORDER BY` của **dòng đầu mỗi granule** ("mark"). Nhỏ đến mức nằm gọn trong RAM.
- Khi lọc, ClickHouse **binary search** trên các mark để loại granule không thể chứa giá trị, rồi đọc *cả* granule còn lại. Đơn vị đọc tối thiểu là 8192 dòng — điểm lookup 1 dòng không nhanh như index B-tree.
- Hệ quả quan trọng: **`ORDER BY` chọn theo cách bạn lọc.** Chỉ cột đầu của khoá loại granule hiệu quả; cột sau chỉ hiệu quả khi cột trước đã thu hẹp. Nguyên tắc: cột lọc `=` thường xuyên và ít giá trị lên trước; cột thời gian thường để sau.

Thử trên hai bảng cùng dữ liệu, khác khoá. `EXPLAIN indexes = 1` cho thấy index nào cắt được bao nhiêu:

```sql ch-local
explain indexes = 1 select count() from play_events where user_id = 42;
```

Trong lần thử: `PrimaryKey ... Granules: 21/376` — chỉ 21 trên 376 granule (mỗi part ~1 granule trúng). Bảng sắp theo `ts` thì không cắt được gì theo `user_id`:

```sql ch-local
explain indexes = 1 select count() from play_events_by_ts where user_id = 42;
```

Đo thật bằng `system.query_log` (cần `system flush logs` để chắc có dữ liệu mới):

```sql ch-local
select count(), sum(amount) from play_events where user_id = 42;
select count(), sum(amount) from play_events_by_ts where user_id = 42;
system flush logs;
select query_duration_ms, read_rows, formatReadableSize(read_bytes) as read_bytes, left(query, 70) as query
from system.query_log
where type = 'QueryFinish' and current_database = currentDatabase()
  and query like '%play_events%' and query not like '%query_log%'
order by event_time desc limit 5;
```

Trong lần thử: bảng `(user_id, ts)` đọc ~158k dòng (~0.6 MiB), bảng `(ts)` đọc ~2.2M dòng (~9.7 MiB) — chênh ~14 lần dòng đọc cho cùng kết quả. **`read_rows`/`read_bytes` là con số cần nhìn**, không phải thời gian (thời gian dao động theo cache).

Lọc theo cột không nằm trong khoá thì quét toàn bộ cột đó:

```sql ch-local
select count() from play_events where payload like 'aaa%';
```

(đọc cả 3M dòng của cột `payload`.) Muốn tránh mà không đổi khoá: **data-skipping index** (`ALTER TABLE ... ADD INDEX ... TYPE minmax|set|bloom_filter GRANULARITY n`) bỏ qua khối dữ liệu theo thống kê nhỏ, hiệu quả khi giá trị cột *tập trung* trong vài khối; **projection** hoặc **materialized view** giữ bản sắp xếp/tổng hợp khác (đổi lại tốn đĩa và ghi).

### Thêm: `PREWHERE`, partition pruning

- `PREWHERE` đọc *trước* các cột trong điều kiện, lọc, rồi mới đọc cột còn lại cho dòng sống sót. ClickHouse thường tự chuyển `WHERE` sang `PREWHERE` (thấy `Prewhere filter` trong `EXPLAIN`); chỉ ép tay khi planner chọn sai.
- Điều kiện trên khoá partition cắt cả part (`Partition ... Parts: 6/21`). Điều kiện trên `ts` ở ví dụ dưới cắt theo min-max/partition/khoá:

```sql ch-local
explain indexes = 1 select count() from play_events where ts > now() - interval 3 day and user_id = 42;
```

## 3. Đọc và đo query

| Công cụ | Dùng để |
| --- | --- |
| `EXPLAIN indexes = 1 <query>` | index nào cắt bao nhiêu granule/part |
| `EXPLAIN PIPELINE <query>` | luồng thực thi song song |
| `system.query_log` | `read_rows`, `read_bytes`, `memory_usage`, thời gian của mọi query đã chạy |
| `system.parts`, `system.columns` | kích thước, số part, nén theo cột |
| `system.processes` | query đang chạy |
| `system.merges`, `system.mutations` | merge/mutation đang chạy |
| `system.part_log` | lịch sử tạo/merge/xoá part |

Query tốn nhất theo tổng đọc (theo dạng chuẩn hoá của câu):

```sql ch-local
select normalized_query_hash, count() as runs, sum(read_rows) as rows, formatReadableSize(sum(read_bytes)) as bytes,
       round(avg(query_duration_ms)) as avg_ms, any(left(query, 80)) as sample
from system.query_log
where type = 'QueryFinish' and event_time > now() - interval 1 day and query_kind = 'Select'
group by normalized_query_hash order by sum(read_bytes) desc limit 10;
```

Join: ClickHouse mặc định **hash join với bảng bên phải nằm gọn RAM** — đặt bảng nhỏ ở bên phải; join hai bảng lớn thì cân nhắc denormalize, dictionary, hoặc join theo khoá sắp xếp. Cẩn thận `SELECT *` (đọc mọi cột — đối nghịch với thế mạnh của cột) và `ORDER BY` không `LIMIT` trên tập lớn.

## 4. "Transaction", ghi đè, xoá

ClickHouse **không phải OLTP**. Những gì thật sự có:

- **Một `INSERT` là nguyên tử ở mức block/part**: một lô vào thành một part (hoặc không vào) — không thấy nửa lô. Với `max_insert_block_size` nhỏ hơn lô, nhiều block có thể vào từng phần; dùng `async_insert` hoặc chia lô có chủ ý khi cần.
- **Đọc nhất quán theo snapshot part**: query dùng tập part tại lúc bắt đầu; merge nền không làm kết quả đứt quãng.
- **Không có isolation giữa nhiều statement**, không `BEGIN/COMMIT` thông dụng (có hỗ trợ thử nghiệm cho transaction trên bảng `MergeTree` với điều kiện bật cấu hình — không dựa vào nó cho thiết kế).
- **Không có khoá dòng, không có deadlock** theo nghĩa Postgres.
- **`UNIQUE` không tồn tại.** Insert hai dòng cùng khoá đều vào. Dedup là việc của engine, bất đồng bộ.

### UPDATE / DELETE là *mutation* — nặng và bất đồng bộ

`ALTER TABLE ... UPDATE/DELETE` **viết lại các part bị ảnh hưởng** ở nền; không thể thu hồi, không rẻ, và có thể chạy lâu khi bảng lớn. Quan sát:

```sql ch-local
alter table play_events update amount = 0 where user_id = 42 settings mutations_sync = 0;
select mutation_id, command, is_done, parts_to_do from system.mutations where table = 'play_events';
```

`mutations_sync = 0` trả về ngay (lúc đó `is_done = 0`, `parts_to_do` > 0 — mutation chạm cả những part không chứa `user_id = 42` vì phải kiểm tra từng part); `= 1` chờ trên server này, `= 2` chờ mọi replica. Bản ClickHouse mới có `DELETE FROM ... WHERE` (lightweight delete): đánh dấu dòng bị xoá, rẻ hơn nhiều, vật lý dọn ở lần merge sau. Quy tắc: nếu cần sửa/xoá thường xuyên từng dòng, thiết kế lại (engine bên dưới) thay vì dùng mutation.

### Engine cho "cập nhật" bằng cách thêm dòng

Thay vì sửa, **ghi thêm phiên bản mới** và để merge gộp:

```sql ch-local
create table play_users (id UInt32, name String, version UInt32)
engine = ReplacingMergeTree(version) order by id;
insert into play_users values (1, 'a', 1);
insert into play_users values (1, 'b', 2);
select count() from play_users;          -- 2: hai part chưa merge, cả hai dòng còn
select count() from play_users final;    -- 1: FINAL gộp lúc đọc (chậm hơn)
```

Sau `optimize table play_users final` thì còn một dòng. Vì dedup chỉ xảy ra khi merge (thời điểm không đoán trước), mọi query cần kết quả đúng phải `FINAL` hoặc dùng `argMax(name, version) ... group by id`. Họ hàng: `SummingMergeTree` (cộng dồn số theo khoá), `AggregatingMergeTree` (giữ trạng thái aggregate, thường kèm materialized view), `CollapsingMergeTree`/`VersionedCollapsingMergeTree` (huỷ cặp +1/−1).

## 5. Checklist tối ưu một query chậm

1. Lấy `read_rows`/`read_bytes`/`memory_usage` từ `system.query_log` — thời gian chỉ là hệ quả.
2. `explain indexes = 1`: `PrimaryKey Granules a/b` — `a ≈ b` nghĩa là khoá không giúp được gì cho điều kiện này.
3. Đổi `ORDER BY` (tạo bảng mới + `INSERT ... SELECT`, không đổi tại chỗ được trừ thêm cột cuối) hoặc thêm **projection/materialized view/skip index** cho mẫu truy vấn thứ hai.
4. Chỉ `SELECT` cột cần; bỏ `SELECT *`.
5. Kiểu dữ liệu nhỏ nhất đủ dùng; `LowCardinality` cho chuỗi ít giá trị khác nhau (nhưng không cho cột hàng triệu giá trị).
6. Số part: nhiều part nhỏ (`system.parts`) = ghi quá nhỏ lẻ; gom lô hoặc dùng `async_insert`.
7. Join: bảng nhỏ bên phải; lọc trước khi join (subquery), cân nhắc dictionary.
8. Aggregate cardinality cao tốn RAM (`max_memory_usage`, `max_bytes_before_external_group_by` cho phép tràn đĩa).
9. Đo lại cùng điều kiện, ghi trước/sau.

## 6. Bài tập

1. Với `play_events`: viết truy vấn "tổng `amount` của `buy` theo `country` trong 7 ngày qua". So `read_rows`/`read_bytes` giữa bảng `(user_id, ts)` và `(ts)`. Bảng nào thắng, vì sao?
2. Tạo bảng thứ ba `order by (event, country, ts)`; truy vấn nào nhanh hơn, truy vấn nào chậm đi? Thử thêm `user_id` ở cuối khoá và xem `EXPLAIN` cắt thế nào.
3. Thêm skip index `set` cho `country` và `minmax` cho `amount`: có làm `Granules` giảm không? Vì sao (gợi ý: dữ liệu mẫu phân bố đều, không tập trung)?
4. `insert` 1000 lần mỗi lần 1 dòng vào một bảng mới, đếm part trong `system.parts` rồi `optimize final`; làm lại bằng một `INSERT` 1000 dòng.
5. Dựng `ReplacingMergeTree` cập nhật cùng `id` 10 lần; viết ba cách đọc phiên bản mới nhất (`FINAL`, `argMax`, `limit 1 by id` sau `order by version desc`) và so `read_rows`.
6. Mutation: `alter table ... delete` rồi `lightweight delete`; so `system.mutations`/`system.part_log` và thời gian.

## Tài liệu tham khảo

Tài liệu chính thức ClickHouse: *MergeTree table engine*, *Sparse Primary Indexes* (hướng dẫn "A Practical Introduction to Primary Indexes"), *Data skipping indexes*, *EXPLAIN*, *Mutations*, *ReplacingMergeTree*, system tables (`query_log`, `parts`, `mutations`).

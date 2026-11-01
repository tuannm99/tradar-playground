# Playground

Đi kèm [tradar](https://github.com/tuannm99/tradar) (client Neovim + `tradar-server`), nhưng nằm ở repo riêng để lớn dần mà không kéo theo bộ cài plugin. Các ví dụ dùng block `.tdb` (`rr` chạy block) — cần bản tradar có file `.tdb` (xem wiki `File-tdb` của tradar); ghi lại phiên bản tối thiểu ở đây khi bạn chốt.

Dữ liệu mẫu và ghi chú về internals, tối ưu query, transaction của từng database, để **thử ngay bằng tradar**: mỗi ví dụ là một block `.tdb` (``` ```sql pg-local ```: đổi `pg-local` thành tên connection của bạn), mở trang này trong Neovim và `rr` trên block. Ưu tiên theo thứ tự dưới đây; đã có Postgres, ClickHouse, Mongo.

| Trang | Trạng thái |
| --- | --- |
| [`postgres.md`](postgres.md) — lưu trữ/MVCC, index, `EXPLAIN`, transaction, checklist | có (ví dụ đã chạy trên Postgres 16) |
| [`clickhouse.md`](clickhouse.md) — MergeTree, part/merge, sparse index, `system.query_log`, mutation | có (ví dụ đã chạy trên ClickHouse thật) |
| [`mongo.md`](mongo.md) — WiredTiger, index, `explain`, ESR, transaction, read/write concern | có (ví dụ đã chạy trên MongoDB 8.3 thật) |

Mọi trang theo cùng khung: (1) mô hình lưu trữ, (2) index và planner, (3) transaction/isolation/lock, (4) checklist tối ưu, (5) bài tập.

Quy ước: số liệu trong trang (thời gian, số dòng) chỉ để minh hoạ — máy bạn sẽ khác; cái cần so sánh là **dạng plan** và **tỉ lệ**.

## Dữ liệu mẫu

[`seed/`](seed/) là một chương trình Go (module riêng, không thuộc workspace Cargo) nạp dữ liệu mẫu cho cả ba DB; mỗi lần chạy xoá rồi tạo lại các bảng/collection `play_*` (Mongo: database `playground`) nên chạy lại được. Cần Go ≥ 1.26 (theo `go.mod`).

```
cd playground/seed
go run .                       # cả ba
go run . postgres mongo        # chọn đích
go run . -pg 'postgres://u:p@host:5432/db' postgres
```

Địa chỉ mặc định (đổi bằng cờ `-pg` `-ch` `-mongo` hoặc biến `PLAY_PG` `PLAY_CH` `PLAY_MONGO`): `postgres://postgres:x@localhost:5432/postgres`, `clickhouse://default@localhost:9000/default` (cổng **native** 9000, khác cổng HTTP 8123 mà tradar dùng; image mới cần `CLICKHOUSE_SKIP_USER_SETUP=1` như lệnh dưới để user `default` không mật khẩu kết nối được từ ngoài container), `mongodb://localhost:27017`.

Dựng nhanh bằng Docker (Mongo cần replica set một node nếu muốn thử transaction):

```
docker run -d --name play-pg -p 5432:5432 -e POSTGRES_PASSWORD=x postgres:16
docker run -d --name play-ch -p 9000:9000 -p 8123:8123 -e CLICKHOUSE_SKIP_USER_SETUP=1 clickhouse/clickhouse-server:latest
docker run -d --name play-mongo -p 27017:27017 mongo:8 --replSet rs0
docker exec play-mongo mongosh --quiet --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})'
```

Kích thước: Postgres 500k đơn, ClickHouse 3 triệu sự kiện, Mongo 300k đơn.

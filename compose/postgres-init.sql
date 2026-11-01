-- Runs once, on first start of the volume (docker-entrypoint-initdb.d).
create extension if not exists pg_stat_statements;

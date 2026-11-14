-- Demo analytics for the ClickHouse MCP service. Synthetic game events, no real people: a
-- service that ships with production data in its seed file is a leak waiting for a `docker
-- compose up`.
--
-- Two users, because the interesting guarantee is not in the Python guard:
--   default        full access, used only by this init script;
--   app_readonly   SELECT on analytics.* with readonly=2, so a write is refused by the server
--                  even if the guard is bypassed. readonly=2 rather than 1 because the service
--                  sends max_execution_time and max_result_rows per query, and readonly=1
--                  refuses to change any setting at all.
CREATE DATABASE IF NOT EXISTS analytics;

CREATE TABLE IF NOT EXISTS analytics.players
(
    id           UInt32,
    country      LowCardinality(String),
    platform     LowCardinality(String),
    installed_at DateTime
) ENGINE = MergeTree ORDER BY id;

CREATE TABLE IF NOT EXISTS analytics.events
(
    ts          DateTime,
    player_id   UInt32,
    event       LowCardinality(String),
    level       UInt16,
    duration_ms UInt32
) ENGINE = MergeTree ORDER BY (ts, player_id);

INSERT INTO analytics.players VALUES
    (1, 'VN', 'android', '2026-10-01 09:00:00'),
    (2, 'VN', 'ios',     '2026-10-02 10:15:00'),
    (3, 'JP', 'android', '2026-10-03 11:30:00'),
    (4, 'VN', 'android', '2026-10-05 19:45:00'),
    (5, 'KR', 'ios',     '2026-10-06 08:20:00'),
    (6, 'VN', 'ios',     '2026-11-01 21:10:00');

INSERT INTO analytics.events VALUES
    ('2026-11-08 09:00:00', 1, 'level_start', 3, 1200),
    ('2026-11-08 09:02:00', 1, 'level_complete', 3, 95000),
    ('2026-11-08 10:00:00', 2, 'level_start', 4, 1500),
    ('2026-11-09 11:00:00', 2, 'level_fail', 4, 41000),
    ('2026-11-09 12:30:00', 3, 'level_start', 3, 1100),
    ('2026-11-09 12:31:00', 3, 'level_complete', 3, 88000),
    ('2026-11-10 08:10:00', 4, 'level_start', 5, 1400),
    ('2026-11-10 08:15:00', 4, 'level_fail', 5, 52000),
    ('2026-11-10 20:00:00', 5, 'shop_open', 0, 3000),
    ('2026-11-11 07:45:00', 1, 'level_start', 6, 1300),
    ('2026-11-11 07:50:00', 1, 'level_complete', 6, 101000),
    ('2026-11-11 21:00:00', 6, 'level_start', 1, 900),
    ('2026-11-12 09:20:00', 6, 'level_fail', 1, 12000),
    ('2026-11-12 09:25:00', 6, 'level_start', 1, 950),
    ('2026-11-13 10:00:00', 2, 'level_start', 7, 1600),
    ('2026-11-13 10:30:00', 2, 'level_complete', 7, 130000),
    ('2026-11-13 18:00:00', 3, 'shop_open', 0, 2500),
    ('2026-11-14 08:00:00', 1, 'level_start', 8, 1500),
    ('2026-11-14 08:02:00', 1, 'level_fail', 8, 60000),
    ('2026-11-14 09:00:00', 4, 'level_start', 6, 1250),
    ('2026-11-14 09:20:00', 4, 'level_complete', 6, 99000),
    ('2026-11-14 10:00:00', 5, 'level_start', 2, 800),
    ('2026-11-14 10:05:00', 5, 'level_complete', 2, 45000),
    ('2026-11-14 11:00:00', 6, 'level_start', 2, 850);

CREATE USER IF NOT EXISTS app_readonly IDENTIFIED WITH plaintext_password BY 'app_readonly_pw' SETTINGS readonly = 2;
GRANT SELECT ON analytics.* TO app_readonly;

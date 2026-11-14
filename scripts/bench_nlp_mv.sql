-- The bench tables for scripts/bench_nlp_mv.py: a raw events table, an AggregatingMergeTree
-- target, and a materialized view that keeps the rollups. Deliberately shaped like production —
-- the interesting question is not "can a model write SQL" but "does it write SQL that still
-- answers the question after the raw table is trimmed".
CREATE DATABASE IF NOT EXISTS bench;

DROP VIEW IF EXISTS bench.events_daily_mv;
DROP TABLE IF EXISTS bench.events_daily;
DROP TABLE IF EXISTS bench.events;
DROP TABLE IF EXISTS bench.players;

CREATE TABLE bench.events
(
    ts          DateTime,
    player_id   UInt32,
    event       LowCardinality(String),
    level       UInt16,
    duration_ms UInt32,
    platform    LowCardinality(String)
) ENGINE = MergeTree ORDER BY (ts, player_id);

CREATE TABLE bench.players
(
    id       UInt32,
    country  LowCardinality(String),
    platform LowCardinality(String)
) ENGINE = MergeTree ORDER BY id;

CREATE TABLE bench.events_daily
(
    day      Date,
    event    LowCardinality(String),
    platform LowCardinality(String),
    events   AggregateFunction(count),
    total_ms AggregateFunction(sum, UInt32),
    avg_ms   AggregateFunction(avg, UInt32)
) ENGINE = AggregatingMergeTree ORDER BY (day, event, platform);

CREATE MATERIALIZED VIEW bench.events_daily_mv TO bench.events_daily AS
SELECT toDate(ts) AS day, event, platform,
       countState()          AS events,
       sumState(duration_ms) AS total_ms,
       avgState(duration_ms) AS avg_ms
FROM bench.events
GROUP BY day, event, platform;

-- 200k synthetic events over 7 days. Inserted after the view exists, which is what a
-- materialized view means: it sees inserts, not history.
INSERT INTO bench.events
SELECT
    toDateTime('2026-11-08 00:00:00') + (number * 3) % 604800                     AS ts,
    (number * 7919) % 5000                                                        AS player_id,
    ['level_start', 'level_complete', 'level_fail', 'shop_open'][1 + number % 4]  AS event,
    toUInt16(1 + number % 12)                                                     AS level,
    toUInt32(200 + (number * 37) % 120000)                                        AS duration_ms,
    ['android', 'ios', 'web'][1 + number % 3]                                     AS platform
FROM numbers(200000);

INSERT INTO bench.players VALUES
    (1, 'VN', 'android'), (2, 'VN', 'ios'), (3, 'JP', 'android'),
    (4, 'VN', 'android'), (5, 'KR', 'ios'), (6, 'VN', 'ios');

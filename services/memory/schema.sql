-- Memory for one platform, many people. Two columns carry the whole design:
--
--   user_id   every statement filters on it, and it comes from a scope object built out of an
--             authenticated identity -- never from a request parameter a model can fill in.
--   expires_at  nothing is kept forever by accident. A row without an expiry is a deliberate
--             decision, not the default of forgetting to make one.
--
-- `key` plus (user_id, kind) is unique, so writing a fact twice replaces it. Memory that only
-- ever grows is not memory, it is a log, and it will outgrow whatever disk you gave it.
--
-- The embedding column is halfvec: same recall as float32 for this use, half the bytes. Rows
-- that are looked up by key (kind='pref') leave it NULL -- embedding them would pay for a
-- similarity search nobody runs.
CREATE TABLE IF NOT EXISTS memory (
    id          bigserial PRIMARY KEY,
    user_id     text        NOT NULL,
    kind        text        NOT NULL,
    key         text        NOT NULL,
    body        text        NOT NULL,
    hash        text        NOT NULL,
    embedding   halfvec({dim}),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    CONSTRAINT memory_body_is_small CHECK (octet_length(body) <= 2048),
    UNIQUE (user_id, kind, key)
);

-- The read path is always "this person's rows, newest or most relevant first".
CREATE INDEX IF NOT EXISTS memory_scope_idx ON memory (user_id, kind, updated_at DESC);
CREATE INDEX IF NOT EXISTS memory_expiry_idx ON memory (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS memory_embedding_idx ON memory USING hnsw (embedding halfvec_cosine_ops);

-- Reading memory is itself an event. Two people can read the same row for very different
-- reasons -- the owner asking about their own crop, and support asking why the owner got that
-- answer -- and only the second one deserves a question later. Logging both, with `actor_id`
-- next to `subject_id`, is what makes the difference visible instead of assumed.
--
-- No content, no body, no query text: an audit table that is interesting to read is a second copy
-- of the data it audits.
CREATE TABLE IF NOT EXISTS memory_event (
    id         bigserial   PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_id   text        NOT NULL,   -- who looked
    subject_id text        NOT NULL,   -- whose memory
    op         text        NOT NULL,   -- get | search | inspect
    rows       integer     NOT NULL,
    run_id     text
);
CREATE INDEX IF NOT EXISTS memory_event_subject_idx ON memory_event (subject_id, at DESC);
CREATE INDEX IF NOT EXISTS memory_event_cross_idx
    ON memory_event (at DESC) WHERE actor_id <> subject_id;

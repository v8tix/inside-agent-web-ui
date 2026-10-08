-- Initial schema: users, sessions, conversations, messages.

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    -- Stored lowercase so the UNIQUE constraint is case-insensitive. The
    -- application normalizes before writing; the CHECK makes a miss fail loudly.
    email         text NOT NULL
                  CONSTRAINT users_email_key UNIQUE
                  CONSTRAINT users_email_normalized CHECK (email <> '' AND email = lower(email)),
    password_hash text NOT NULL CONSTRAINT users_password_hash_present CHECK (password_hash <> ''),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- id holds the SHA-256 hex digest of the cookie token, never the token itself,
-- so a leaked table cannot be replayed as live sessions.
CREATE TABLE sessions (
    id         text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE conversations (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title      text NOT NULL CONSTRAINT conversations_title_length CHECK (char_length(title) BETWEEN 1 AND 120),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- The sidebar lists a user's conversations, newest activity first.
CREATE INDEX conversations_user_updated_idx ON conversations (user_id, updated_at DESC);

CREATE TABLE messages (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             integer NOT NULL CONSTRAINT messages_seq_positive CHECK (seq > 0),
    role            text NOT NULL CONSTRAINT messages_role_known CHECK (role IN ('user', 'assistant')),
    -- Markdown, exactly as the agent produced it (or the user typed it).
    content         text NOT NULL DEFAULT '',
    status          text NOT NULL
                    CONSTRAINT messages_status_known CHECK (status IN ('pending', 'streaming', 'complete', 'stopped', 'failed')),
    error           text NOT NULL DEFAULT '',
    -- ttft_ms, duration_ms, usage {prompt, completion}, tool_calls []
    meta            jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT messages_conversation_seq_key UNIQUE (conversation_id, seq)
);

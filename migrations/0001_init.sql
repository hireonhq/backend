-- HireOn schema. IDs for seeded reference data are preserved verbatim from the
-- Mongo ObjectId hex strings so existing user bookmarks, ML embeddings (keyed by
-- user/job id) and the frontend contract keep working after migration.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE company_industries (
    id         text PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE companies (
    id              text PRIMARY KEY,
    name            text NOT NULL,
    description     text NOT NULL,
    profile_src     text NOT NULL,
    location        text NOT NULL,
    employees_count text,
    industry_id     text NOT NULL REFERENCES company_industries (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX companies_industry_id_idx ON companies (industry_id);
CREATE INDEX companies_name_idx ON companies (name);

CREATE TABLE job_categories (
    id         text PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE job_min_experiences (
    id         text PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE jobs (
    id                    text PRIMARY KEY,
    url                   text NOT NULL,
    company_id            text NOT NULL REFERENCES companies (id),
    job_position          text NOT NULL,
    employment_type       text NOT NULL,
    working_location_type text NOT NULL,
    working_location      text,
    min_experience_id     text NOT NULL REFERENCES job_min_experiences (id),
    min_salary            bigint NOT NULL,
    max_salary            bigint NOT NULL,
    job_desc_list         text[] NOT NULL,
    job_qualifications_list text[] NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
-- Column order matters for the two-column indexes: these back the /jobs filters.
-- The old Mongo text index referenced jobPosition/employmentType/categories/
-- minExperience/salary, two of which did not exist; these replace it.
CREATE INDEX jobs_company_id_idx ON jobs (company_id);
CREATE INDEX jobs_min_experience_id_idx ON jobs (min_experience_id);
CREATE INDEX jobs_created_at_idx ON jobs (created_at DESC);
CREATE INDEX jobs_salary_idx ON jobs (min_salary, max_salary);

-- Categories are a real many-to-many; the old schema stored ObjectId strings in a
-- `[String]` field, which broke the declared text index and left no referential
-- integrity. This join table is the fix.
CREATE TABLE job_category_links (
    job_id      text NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    category_id text NOT NULL REFERENCES job_categories (id),
    -- Mongo stored categories as an ordered array and populate() returned them in
    -- that order; position preserves it so responses stay byte-identical.
    position    int  NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, category_id)
);
CREATE INDEX job_category_links_category_id_idx ON job_category_links (category_id);

CREATE TABLE users (
    id             text PRIMARY KEY,
    username       text NOT NULL UNIQUE,
    email          text NOT NULL UNIQUE,
    supabase_id    text UNIQUE,
    password       text,
    photo_url      text,
    role           text NOT NULL DEFAULT 'seeker' CHECK (role IN ('admin', 'seeker')),
    oauth_provider text,
    verified_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE profiles (
    id              text PRIMARY KEY,
    user_id         text NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    fullname        text NOT NULL,
    phone           text,
    domicile        text,
    last_education  text,
    photo_url       text,
    portfolio_url   text,
    cv_url          text,
    cv_status       text NOT NULL DEFAULT 'none' CHECK (cv_status IN ('none','pending','ready','failed')),
    tag_preferences text[] NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bookmark_jobs (
    profile_id text NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
    job_id     text NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_id, job_id)
);
CREATE INDEX bookmark_jobs_job_id_idx ON bookmark_jobs (job_id);

CREATE TABLE analysis_results (
    id                       text PRIMARY KEY,
    user_id                  text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_id                   text NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    cv_relevance_score       double precision NOT NULL,
    explanation              text NOT NULL,
    skil_identification_dict jsonb NOT NULL,
    suggestions              text[] NOT NULL,
    improvements             text[] NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, job_id)
);

-- Embeddings live next to the rows they describe, so an index update is in the
-- same transaction as the job write. 256d per EmbeddingGemma 2's MRL truncation:
-- 61.17 MTEB multilingual vs 61.36 at 768d, for 3x less storage.
CREATE TABLE job_embeddings (
    job_id     text PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    model      text NOT NULL,
    embedding  vector(256) NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_cv_embeddings (
    user_id    text PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    model      text NOT NULL,
    embedding  vector(256) NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Async job tracking for CV upload and cover letter generation.
CREATE TABLE jobs_queue (
    id         text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('cv_embedding', 'cv_analysis', 'cover_letter')),
    status     text NOT NULL CHECK (status IN ('pending','running','done','failed')),
    attempts   int NOT NULL DEFAULT 0,
    error      text,
    result     jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_queue_user_id_created_at_idx ON jobs_queue (user_id, created_at DESC);
CREATE INDEX jobs_queue_status_idx ON jobs_queue (status) WHERE status IN ('pending', 'running');

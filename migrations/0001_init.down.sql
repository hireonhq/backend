-- Reverts 0001_init.up.sql.
--
-- pgvector is not dropped: other schemas in the same database may rely on it, and
-- dropping an extension is not reversible by re-running the migration.
DROP TABLE IF EXISTS jobs_queue;
DROP TABLE IF EXISTS user_cv_embeddings;
DROP TABLE IF EXISTS job_embeddings;
DROP TABLE IF EXISTS analysis_results;
DROP TABLE IF EXISTS bookmark_jobs;
DROP TABLE IF EXISTS profiles;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS job_category_links;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS job_min_experiences;
DROP TABLE IF EXISTS job_categories;
DROP TABLE IF EXISTS companies;
DROP TABLE IF EXISTS company_industries;

-- Runs once, when the Docker volume is first created.
-- Integration tests truncate tables, so they get their own database.
CREATE DATABASE sitewatch_test OWNER sitewatch;

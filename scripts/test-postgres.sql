-- Run as the administrator of a fresh, disposable PostgreSQL test instance.
REVOKE CONNECT ON DATABASE postgres, template0, template1 FROM PUBLIC;
CREATE ROLE dbbranch_manager LOGIN CREATEDB CREATEROLE PASSWORD 'test-manager-password';
GRANT pg_signal_backend TO dbbranch_manager;
CREATE DATABASE dbbranch_admin OWNER dbbranch_manager;
REVOKE CONNECT ON DATABASE dbbranch_admin FROM PUBLIC;

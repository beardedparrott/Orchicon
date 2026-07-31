-- Down migration for 20260731000016_ask_orchicon.sql

DROP TABLE IF EXISTS ask_orchicon_conversations CASCADE;
DROP TABLE IF EXISTS ask_orchicon_messages CASCADE;
DROP INDEX IF EXISTS ;
DROP TABLE IF EXISTS ask_orchicon_agent_config CASCADE;

-- Backfill orchicon mode conversations to brainstorm (ADR-6)
UPDATE ask_orchicon_conversations SET mode='brainstorm' WHERE mode='orchicon';

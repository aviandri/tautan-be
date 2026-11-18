ALTER TABLE email_outbox DROP COLUMN IF EXISTS reply_to;
ALTER TABLE users DROP COLUMN IF EXISTS notify_submissions;

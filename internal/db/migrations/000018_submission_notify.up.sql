-- Owner notifications for form and contact submissions (on by default).
ALTER TABLE users ADD COLUMN notify_submissions boolean NOT NULL DEFAULT true;
-- Reply-To for notification emails (the visitor's address).
ALTER TABLE email_outbox ADD COLUMN reply_to text NOT NULL DEFAULT '';

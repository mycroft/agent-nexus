-- The model a job ran on, set when it is claimed.
ALTER TABLE jobs ADD COLUMN model TEXT NOT NULL DEFAULT '';

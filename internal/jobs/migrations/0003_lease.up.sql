-- Who holds a started job: the runner (empty for the controller's own
-- workers) and the claim's random token. Every write of the holder
-- carries the token, which the claim, Finish and recovery change, so a
-- worker that lost its job cannot write to it. Model slots and group
-- exclusivity count the jobs whose lease is held.
ALTER TABLE jobs ADD COLUMN runner TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN lease TEXT;
-- When a runner's lease expires unless it renews it. NULL for the
-- controller's own workers, whose jobs are recovered at startup.
ALTER TABLE jobs ADD COLUMN lease_until TEXT;

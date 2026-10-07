-- +goose Up
-- +goose StatementBegin

-- CIC-118: persist a step's `if:` conditional (success/failure/always, an empty
-- value meaning "success") so it can travel server -> queue -> runner and be
-- enforced at execution time. The runner evaluates it against the outcomes of
-- the steps that ran before it in the same job.
ALTER TABLE steps
	ADD COLUMN if_condition text NOT NULL DEFAULT '';

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

ALTER TABLE steps
	DROP COLUMN IF EXISTS if_condition;

-- +goose StatementEnd

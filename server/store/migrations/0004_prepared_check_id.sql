-- +goose Up
-- +goose StatementBegin

-- CIC-81: the forge's check ID for a run, recorded after forge.Prepare
-- succeeds, so the run-terminal hook can complete that same check.
--
-- Nullable on purpose: NULL means "no check was prepared" (NoOp forge,
-- Prepare failed, or a run created before this migration). The run-terminal
-- hook skips forge.Complete for those rather than PATCHing check-runs/0.
-- Only RunCheckRef reads it; runSelectColumns does not, so the runs API is
-- unaffected by the NULLs.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS prepared_check_id bigint;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runs
    DROP COLUMN IF EXISTS prepared_check_id;

-- +goose StatementEnd

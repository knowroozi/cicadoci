package jobtoken

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/audit"
)

// AuditLog is the production Auditor for jobtoken. It writes token
// mint/use/revoke events to an audit.Log.
//
// It is the concrete type passed to NewManager in production wiring
// (main.go); MemoryAuditor is used only in tests.
//
// The three audit entry types it writes:
//   - action jobtoken.issue  — token ID, run ID, job ID, scopes
//   - action jobtoken.use    — token ID, run ID, job ID, scope exercised
//   - action jobtoken.revoke — token ID, timestamp
//
// A nil slog.Logger falls back to slog.Default().
type AuditLog struct {
	log  audit.Log
	slog *slog.Logger
}

// NewAuditLog builds an AuditLog backed by l and logger.
// A nil logger is replaced with slog.Default().
func NewAuditLog(l audit.Log, logger *slog.Logger) *AuditLog {
	if logger == nil {
		logger = slog.Default()
	}
	return &AuditLog{log: l, slog: logger}
}

func (a *AuditLog) Issue(ctx context.Context, t Token) error {
	_, err := a.log.Append(ctx, audit.Entry{
		Action:        "jobtoken.issue",
		PrincipalType: audit.PrincipalSystem, // issuer mints the job token, a system action
		Surface:       audit.SurfaceSystem,
		Actor:         t.Claims.Issuer,
		Target:        fmt.Sprintf("run:%s job:%s", t.Claims.RunID, t.Claims.JobID),
		Reason:        fmt.Sprintf("scopes=%v", t.Claims.Scopes),
		Timestamp:     t.IssuedAt,
	})
	if err != nil {
		a.slog.Warn("jobtoken audit issue failed",
			"token_id", t.ID, "run_id", t.Claims.RunID, "err", err)
	}
	return err
}

func (a *AuditLog) Use(ctx context.Context, t Token) error {
	_, err := a.log.Append(ctx, audit.Entry{
		Action:        "jobtoken.use",
		PrincipalType: audit.PrincipalToken, // the exercising principal is the job's token itself
		Surface:       audit.SurfaceSystem,
		Actor:         t.ID.String(), // the token's own uuid, not the server-side Issuer
		Target:        fmt.Sprintf("run:%s job:%s", t.Claims.RunID, t.Claims.JobID),
		Reason:        "verify",
		Timestamp:     time.Now(),
	})
	if err != nil {
		a.slog.Warn("jobtoken audit use failed",
			"token_id", t.ID, "err", err)
	}
	return err
}

func (a *AuditLog) Revoke(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := a.log.Append(ctx, audit.Entry{
		Action:        "jobtoken.revoke",
		PrincipalType: audit.PrincipalSystem, // revocation is a system action
		Surface:       audit.SurfaceSystem,
		Actor:         audit.PrincipalSystem,
		Target:        fmt.Sprintf("token:%s", id),
		Reason:        "revoked",
		Timestamp:     at,
	})
	if err != nil {
		a.slog.Warn("jobtoken audit revoke failed",
			"token_id", id, "err", err)
	}
	return err
}

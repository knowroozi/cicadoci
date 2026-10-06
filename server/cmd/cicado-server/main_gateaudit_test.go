// Copyright 2024 Kaveh Nowroozi. Apache-2.0.
//
// Pure per-state unit tests for gateAuditEntry (CIC-192 A1): every terminal gate
// state must map to a meaningful principal_type + surface, not the "user"/"api"
// default.

package main

import (
	"testing"

	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/audit"
)

func TestGateAuditEntry_PerState(t *testing.T) {
	cases := []struct {
		name          string
		state         approvalgates.GateState
		actorID       string
		wantAction    audit.Action
		wantPrincipal string
		wantSurface   string
		wantActor     string
	}{
		{"approved", approvalgates.GateApproved, "u-1", audit.ActionApprove, audit.PrincipalUser, audit.SurfaceChat, "u-1"},
		{"denied", approvalgates.GateDenied, "u-2", audit.ActionDeny, audit.PrincipalUser, audit.SurfaceChat, "u-2"},
		// A timeout has no acting user: a system event.
		{"timed_out", approvalgates.GateTimedOut, "", audit.ActionGateTimeout, audit.PrincipalSystem, audit.SurfaceSystem, "system"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := gateAuditEntry("run-1", tc.state, tc.actorID, "")
			if e.Action != tc.wantAction {
				t.Errorf("action = %v, want %v", e.Action, tc.wantAction)
			}
			if e.PrincipalType != tc.wantPrincipal {
				t.Errorf("principal_type = %q, want %q", e.PrincipalType, tc.wantPrincipal)
			}
			if e.Surface != tc.wantSurface {
				t.Errorf("surface = %q, want %q", e.Surface, tc.wantSurface)
			}
			if e.Actor != tc.wantActor {
				t.Errorf("actor = %q, want %q", e.Actor, tc.wantActor)
			}
			if e.Target != "run-1" {
				t.Errorf("target = %q, want %q", e.Target, "run-1")
			}
		})
	}

	// A user state with a blank actor must still fall back to "system" so the
	// principal_id column is never blank.
	if e := gateAuditEntry("run-1", approvalgates.GateApproved, "", ""); e.Actor != "system" {
		t.Errorf("blank actor on an approved gate: actor = %q, want %q", e.Actor, "system")
	}
}

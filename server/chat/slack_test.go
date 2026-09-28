package chat

import "testing"

func TestSendMessage(t *testing.T) {
	p := NewSlackProvider()
	id, err := p.SendMessage(t.Context(), "#ci", MessagePayload{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(id) != "msg-1" {
		t.Errorf("expected msg-1, got %q", id)
	}
	records := p.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Channel != "#ci" || records[0].Payload.Text != "hello" {
		t.Fatalf("unexpected record: %+v", records[0])
	}
}

func TestUpdateMessage(t *testing.T) {
	p := NewSlackProvider()
	id, _ := p.SendMessage(t.Context(), "#ci", MessagePayload{Text: "original"})
	err := p.UpdateMessage(t.Context(), "#ci", string(id), MessagePayload{Text: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	records := p.Records()
	if records[0].Payload.Text != "updated" || records[0].Action != ActionUpdate {
		t.Fatalf("unexpected: %+v", records[0])
	}
}

func TestUpdateMessageNotFound(t *testing.T) {
	p := NewSlackProvider()
	err := p.UpdateMessage(t.Context(), "#ci", "nonexistent", MessagePayload{Text: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestOpenThread(t *testing.T) {
	p := NewSlackProvider()
	// First send a top-level message to get a parent ID.
	parentID, _ := p.SendMessage(t.Context(), "#ci", MessagePayload{Text: "parent"})
	childID, err := p.OpenThread(t.Context(), "#ci", string(parentID), MessagePayload{Text: "child"})
	if err != nil {
		t.Fatal(err)
	}
	records := p.Records()
	if len(records) != 2 || records[1].ParentID != string(parentID) {
		t.Fatalf("unexpected: %+v", records)
	}
	if records[1].MsgID != childID {
		t.Fatalf("expected child id %q, got %q", childID, records[1].MsgID)
	}
}

func TestRequestApproval(t *testing.T) {
	p := NewSlackProvider()
	triggerID, callbackID, err := p.RequestApproval(t.Context(), "#ci", "msg-1", ApprovalRequest{
		RunID:        "run-1",
		Reason:       "Approve deployment?",
		Allow:        "approve",
		Deny:         "deny",
		TimeoutSec:   3600,
		AllowedUsers: []string{"user1", "user2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if triggerID == "" || callbackID != "approve" {
		t.Fatalf("unexpected triggerID=%q callbackID=%q", triggerID, callbackID)
	}
	records := p.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if len(records[0].Payload.Buttons) != 2 {
		t.Errorf("expected 2 buttons, got %d", len(records[0].Payload.Buttons))
	}
	if records[0].Payload.Buttons[0].ActionID != "approve" {
		t.Errorf("expected approve button first, got %q", records[0].Payload.Buttons[0].ActionID)
	}
}

func TestCloseApproval(t *testing.T) {
	p := NewSlackProvider()
	_, _, _ = p.RequestApproval(t.Context(), "#ci", "msg-1", ApprovalRequest{
		RunID: "run-1", Allow: "approve", Deny: "deny",
	})
	err := p.CloseApproval(t.Context(), "#ci", "msg-1", ApprovalApproved)
	if err != nil {
		t.Fatal(err)
	}
	records := p.Records()
	if records[0].Action != ActionClose {
		t.Fatalf("expected close action, got %q", records[0].Action)
	}
	if records[0].Status != ApprovalApproved {
		t.Fatalf("expected status ApprovalApproved, got %v", records[0].Status)
	}
}

func TestCloseApprovalRecordsDenied(t *testing.T) {
	p := NewSlackProvider()
	_, _, _ = p.RequestApproval(t.Context(), "#ci", "msg-1", ApprovalRequest{
		RunID: "run-2", Allow: "approve", Deny: "deny",
	})
	if err := p.CloseApproval(t.Context(), "#ci", "msg-1", ApprovalDenied); err != nil {
		t.Fatal(err)
	}
	if status := p.Records()[0].Status; status != ApprovalDenied {
		t.Fatalf("expected ApprovalDenied, got %v", status)
	}
}

func TestCloseApprovalRecordsTimedOut(t *testing.T) {
	p := NewSlackProvider()
	_, _, _ = p.RequestApproval(t.Context(), "#ci", "msg-1", ApprovalRequest{
		RunID: "run-3", Allow: "approve", Deny: "deny",
	})
	if err := p.CloseApproval(t.Context(), "#ci", "msg-1", ApprovalTimedOut); err != nil {
		t.Fatal(err)
	}
	if status := p.Records()[0].Status; status != ApprovalTimedOut {
		t.Fatalf("expected ApprovalTimedOut, got %v", status)
	}
}

func TestCloseApprovalNotFound(t *testing.T) {
	p := NewSlackProvider()
	err := p.CloseApproval(t.Context(), "#ci", "nonexistent", ApprovalApproved)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

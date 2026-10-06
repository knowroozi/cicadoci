package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIssuesWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte("\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	iss, err := loadIssues(path)
	if err != nil {
		t.Fatalf("loadIssues error: %v", err)
	}
	if len(iss) != 0 {
		t.Fatalf("expected empty slice, got %d items", len(iss))
	}
}

func writeTempLog(t *testing.T, issues []Issue) string {
	t.Helper()
	data, err := json.MarshalIndent(issues, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	dir := t.TempDir()
	path := filepath.Join(dir, "log.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestRunPaginationAndDeterminism(t *testing.T) {
	// Page 1 response (numeric priority, with labels and attachment)
	page1 := `{"data":{"issues":{"nodes":[{"identifier":"CIC-001","title":"Ticket 1","description":"Desc","state":{"type":"Done"},"completedAt":"2026-09-30T12:00:00Z","createdAt":"2026-09-01T08:00:00Z","labels":{"nodes":[{"name":"backend"}]},"priority":2,"priorityLabel":"Medium","assignee":{"name":"alice"},"project":{"name":"projA"},"projectMilestone":{"name":"milestone1"},"url":"https://linear.app/issue/CIC-001","attachments":{"nodes":[{"url":"https://github.com/pr/1","sourceType":"GitHub","title":"PR 1"}]}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor2"}}}}
`
	// Page 2 response (null assignee/project/milestone, empty labels, no attachments, priority 0)
	page2 := `{"data":{"issues":{"nodes":[{"identifier":"CIC-002","title":"Ticket 2","description":"Desc2","state":{"type":"Done"},"completedAt":"2026-10-01T09:00:00Z","createdAt":"2026-09-15T10:00:00Z","labels":{"nodes":[]},"priority":0,"priorityLabel":"None","assignee":null,"project":null,"projectMilestone":null,"url":"https://linear.app/issue/CIC-002","attachments":{"nodes":[]}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}
`
	// Set up test server to return pages sequentially
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		if call == 1 {
			w.Write([]byte(page1))
		} else {
			w.Write([]byte(page2))
		}
	}))
	defer srv.Close()

	fetch := func() ([]Issue, error) {
		return fetchIssues(srv.URL, "dummy-token")
	}

	// Start with empty log file
	logPath := writeTempLog(t, []Issue{})
	defer os.Remove(logPath)

	// First run
	if err := run(logPath, fetch); err != nil {
		t.Fatalf("run1: %v", err)
	}
	out1, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read1: %v", err)
	}
	// Second run (idempotent)
	if err := run(logPath, fetch); err != nil {
		t.Fatalf("run2: %v", err)
	}
	out2, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read2: %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("outputs differ after second run\n%s\n---\n%s", out1, out2)
	}
	// Verify content
	var issues []Issue
	if err := json.Unmarshal(out1, &issues); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	// Ensure ordering by completedAt then identifier
	if issues[0].Identifier != "CIC-001" || issues[1].Identifier != "CIC-002" {
		t.Fatalf("unexpected order: %v", issues)
	}
	// Verify labels sorted (single label test)
	if len(issues[0].Labels) != 1 || issues[0].Labels[0] != "backend" {
		t.Fatalf("labels not as expected: %v", issues[0].Labels)
	}
	// Verify PRLinks sorted
	if len(issues[0].PRLinks) != 1 || issues[0].PRLinks[0] != "https://github.com/pr/1" {
		t.Fatalf("prLinks not as expected: %v", issues[0].PRLinks)
	}
	// Verify priority int and label
	if issues[0].Priority != 2 || issues[0].PriorityLabel != "Medium" {
		t.Fatalf("priority mismatch: %+v", issues[0])
	}
	// Verify null fields become empty strings
	if issues[1].Assignee != "" || issues[1].Project != "" || issues[1].Milestone != "" {
		t.Fatalf("null fields not empty: %+v", issues[1])
	}
}

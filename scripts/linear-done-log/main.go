package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

type Issue struct {
	Identifier    string   `json:"identifier"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	State         string   `json:"state"`
	CompletedAt   string   `json:"completedAt"`
	CreatedAt     string   `json:"createdAt"`
	Labels        []string `json:"labels"`
	Priority      int      `json:"priority"`
	PriorityLabel string   `json:"priorityLabel"`
	Assignee      string   `json:"assignee"`
	Project       string   `json:"project"`
	Milestone     string   `json:"milestone"`
	URL           string   `json:"url"`
	PRLinks       []string `json:"prLinks"`
}

func loadIssues(path string) ([]Issue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Issue{}, nil
		}
		return nil, err
	}
	// Trim whitespace; treat empty or whitespace‑only as empty list
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return []Issue{}, nil
	}
	var issues []Issue
	if err := json.Unmarshal(trimmed, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func saveIssues(path string, issues []Issue) error {
	data, err := json.MarshalIndent(issues, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func dedupeAndSort(existing []Issue, newIssues []Issue) []Issue {
	seen := make(map[string]Issue)
	for _, iss := range existing {
		seen[iss.Identifier] = iss
	}
	for _, iss := range newIssues {
		if _, ok := seen[iss.Identifier]; !ok {
			seen[iss.Identifier] = iss
		}
	}
	combined := make([]Issue, 0, len(seen))
	for _, iss := range seen {
		// Ensure non-nil slices
		if iss.Labels == nil {
			iss.Labels = []string{}
		}
		if iss.PRLinks == nil {
			iss.PRLinks = []string{}
		}
		// Sort labels and PRLinks for determinism
		sort.Strings(iss.Labels)
		sort.Strings(iss.PRLinks)
		combined = append(combined, iss)
	}
	sort.Slice(combined, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, combined[i].CompletedAt)
		tj, _ := time.Parse(time.RFC3339, combined[j].CompletedAt)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return combined[i].Identifier < combined[j].Identifier
	})
	return combined
}

// fetchIssues queries Linear GraphQL API, handling pagination.
func fetchIssues(baseURL, apiKey string) ([]Issue, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("LINEAR_API_KEY is empty")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var all []Issue
	cursor := ""
	for {
		// Build request payload struct for safety
		payload := struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}{
			Query:     `query ($cursor: String) { issues(filter: {state: {type: {eq: "completed"}}}, first: 100, after: $cursor) { nodes { identifier title description state { type } completedAt createdAt labels { nodes { name } } priority priorityLabel assignee { name } project { name } projectMilestone { name } url attachments { nodes { url sourceType title } } } pageInfo { hasNextPage endCursor } } }`,
			Variables: map[string]interface{}{"cursor": nil},
		}
		if cursor != "" {
			payload.Variables["cursor"] = cursor
		}
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(context.Background(), "POST", baseURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		// Ensure body closed each iteration
		body, readErr := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			return nil, errClose
		}
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			snippet := string(body)
			if len(snippet) > 300 {
				snippet = snippet[:300]
			}
			return nil, fmt.Errorf("linear api returned status %d: %s", resp.StatusCode, snippet)
		}
		var respData struct {
			Data struct {
				Issues struct {
					Nodes []struct {
						Identifier  string `json:"identifier"`
						Title       string `json:"title"`
						Description string `json:"description"`
						State       struct {
							Type string `json:"type"`
						} `json:"state"`
						CompletedAt string `json:"completedAt"`
						CreatedAt   string `json:"createdAt"`
						Labels      struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
						Priority      int    `json:"priority"`
						PriorityLabel string `json:"priorityLabel"`
						Assignee      struct {
							Name string `json:"name"`
						} `json:"assignee"`
						Project struct {
							Name string `json:"name"`
						} `json:"project"`
						ProjectMilestone struct {
							Name string `json:"name"`
						} `json:"projectMilestone"`
						URL         string `json:"url"`
						Attachments struct {
							Nodes []struct {
								URL        string `json:"url"`
								SourceType string `json:"sourceType"`
								Title      string `json:"title"`
							} `json:"nodes"`
						} `json:"attachments"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issues"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &respData); err != nil {
			return nil, err
		}
		if len(respData.Errors) > 0 {
			return nil, fmt.Errorf("linear api errors: %s", respData.Errors[0].Message)
		}
		for _, n := range respData.Data.Issues.Nodes {
			// collect labels
			var lbls []string
			for _, l := range n.Labels.Nodes {
				lbls = append(lbls, l.Name)
			}
			// collect PR links from attachments (store all attachment URLs)
			var prLinks []string
			for _, a := range n.Attachments.Nodes {
				prLinks = append(prLinks, a.URL)
			}
			// sort within dedupeAndSort later
			issue := Issue{
				Identifier:    n.Identifier,
				Title:         n.Title,
				Description:   n.Description,
				State:         n.State.Type,
				CompletedAt:   n.CompletedAt,
				CreatedAt:     n.CreatedAt,
				Labels:        lbls,
				Priority:      n.Priority,
				PriorityLabel: n.PriorityLabel,
				Assignee:      n.Assignee.Name,
				Project:       n.Project.Name,
				Milestone:     n.ProjectMilestone.Name,
				URL:           n.URL,
				PRLinks:       prLinks,
			}
			all = append(all, issue)
		}
		if !respData.Data.Issues.PageInfo.HasNextPage {
			break
		}
		cursor = respData.Data.Issues.PageInfo.EndCursor
	}
	return all, nil
}

func run(logPath string, fetch func() ([]Issue, error)) error {
	existing, err := loadIssues(logPath)
	if err != nil {
		return err
	}
	newIssues, err := fetch()
	if err != nil {
		return err
	}
	combined := dedupeAndSort(existing, newIssues)
	return saveIssues(logPath, combined)
}

func main() {
	apiKey := os.Getenv("LINEAR_API_KEY")
	baseURL := "https://api.linear.app/graphql"
	logPath := "docs/linear-done-log.json"
	err := run(logPath, func() ([]Issue, error) { return fetchIssues(baseURL, apiKey) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

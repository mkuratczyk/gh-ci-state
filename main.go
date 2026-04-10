// Reports CI workflow run history with a compact per-run trend and latest-run details.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

const (
	iconRunning   = "⏳"
	iconSuccess   = "✅"
	iconFailure   = "❌"
	iconCancelled = "⚪"
)

func main() {
	os.Exit(run(context.Background()))
}

func run(ctx context.Context) int {
	fs := flag.NewFlagSet("gh-ci-state", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: gh ci-state -workflow <file-or-id> [options]\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Show the last N workflow runs as a trend (oldest→newest) and the latest run's job summary.\n\n")
		fs.PrintDefaults()
	}

	var (
		repoStr    = fs.String("R", "", "repository `[HOST/]OWNER/REPO` (recommended for GHE; default: GH_REPO or git cwd)")
		workflow   = fs.String("workflow", "", "`workflow` file name or numeric ID (required)")
		branch     = fs.String("branch", "", "`branch` (default: repository default branch)")
		jsonOutput = fs.Bool("json", false, "emit machine-readable JSON")
		chatOutput = fs.Bool("chat", false, "Google Chat line: icons + <run-URL|summary> (mutually exclusive with -json)")
		runCount   = *fs.Int("runs", 5, "max recent `runs` in the trend (fewer if the branch has less history; none: no-runs message)")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *workflow == "" {
		fmt.Fprintln(os.Stderr, "error: -workflow is required")
		fs.Usage()
		return 2
	}
	if *jsonOutput && *chatOutput {
		fmt.Fprintln(os.Stderr, "error: -json and -chat are mutually exclusive")
		return 2
	}
	if runCount < 1 {
		fmt.Fprintln(os.Stderr, "error: -runs must be at least 1")
		return 2
	}

	repo, err := resolveRepo(*repoStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client, err := api.NewRESTClient(api.ClientOptions{Host: repo.Host})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	br := strings.TrimSpace(*branch)
	if br == "" {
		br, err = defaultBranch(ctx, client, repo.Owner, repo.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not resolve default branch: %v\n", err)
			return 1
		}
	}

	runs, err := listWorkflowRuns(ctx, client, repo.Owner, repo.Name, *workflow, br, runCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if len(runs) == 0 {
		return emitNoWorkflowRuns(repo.Owner, repo.Name, repo.Host, *workflow, br, *jsonOutput, *chatOutput)
	}

	latest := runs[len(runs)-1]
	passed, total, err := countJobs(ctx, client, repo.Owner, repo.Name, latest.ID, latest.Conclusion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: jobs: %v\n", err)
		return 1
	}

	if *jsonOutput {
		out := struct {
			Repo     string   `json:"repo"`
			Host     string   `json:"host"`
			Workflow string   `json:"workflow"`
			Branch   string   `json:"branch"`
			Trend    []string `json:"trend_conclusions"`
			Latest   struct {
				ID         int64   `json:"id"`
				HtmlURL    string  `json:"html_url"`
				Summary    string  `json:"summary"`
				Conclusion *string `json:"conclusion"`
				UpdatedAt  string  `json:"updated_at"`
				PassedJobs int     `json:"passed_jobs"`
				TotalJobs  int     `json:"total_jobs"`
			} `json:"latest"`
		}{}
		out.Repo = repo.Owner + "/" + repo.Name
		out.Host = repo.Host
		out.Workflow = *workflow
		out.Branch = br
		for _, r := range runs {
			c := ""
			if r.Conclusion != nil {
				c = *r.Conclusion
			}
			out.Trend = append(out.Trend, c)
		}
		out.Latest.ID = latest.ID
		out.Latest.HtmlURL = latestRunWebURL(repo.Host, repo.Owner, repo.Name, latest.ID, latest.HtmlURL)
		out.Latest.Conclusion = latest.Conclusion
		out.Latest.UpdatedAt = latest.UpdatedAt
		out.Latest.PassedJobs = passed
		out.Latest.TotalJobs = total
		updatedAtJSON, err := time.Parse(time.RFC3339, latest.UpdatedAt)
		if err != nil {
			updatedAtJSON = time.Time{}
		}
		out.Latest.Summary = formatSummary(latest.Conclusion, passed, total, timeAgo(updatedAtJSON))
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}

	var trend strings.Builder
	trend.Grow(len(runs) * 3)
	for _, r := range runs {
		trend.WriteString(iconForConclusion(r.Conclusion))
	}

	updatedAt, err := time.Parse(time.RFC3339, latest.UpdatedAt)
	if err != nil {
		updatedAt = time.Time{}
	}
	age := timeAgo(updatedAt)
	summary := formatSummary(latest.Conclusion, passed, total, age)

	if *chatOutput {
		runURL := latestRunWebURL(repo.Host, repo.Owner, repo.Name, latest.ID, latest.HtmlURL)
		if runURL == "" {
			fmt.Println(trend.String() + " " + summary)
			return 0
		}
		label := strings.ReplaceAll(summary, "|", "·")
		fmt.Printf("%s <%s|%s>\n", trend.String(), runURL, label)
		return 0
	}

	fmt.Println(trend.String() + " " + summary)
	return 0
}

// emitNoWorkflowRuns handles a branch/workflow with no Actions runs yet (exit 0, sensible message).
func emitNoWorkflowRuns(owner, name, host, workflow, branch string, jsonOut, chatOut bool) int {
	const msg = "no workflow runs for this branch"
	if jsonOut {
		out := map[string]interface{}{
			"repo":              owner + "/" + name,
			"host":              host,
			"workflow":          workflow,
			"branch":            branch,
			"trend_conclusions": []string{},
			"latest": map[string]interface{}{
				"id":          nil,
				"html_url":    "",
				"summary":     msg,
				"conclusion":  nil,
				"updated_at":  "",
				"passed_jobs": 0,
				"total_jobs":  0,
			},
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	if chatOut {
		fmt.Println(msg)
		return 0
	}
	fmt.Println(msg)
	return 0
}

func resolveRepo(explicit string) (repository.Repository, error) {
	if strings.TrimSpace(explicit) != "" {
		return repository.Parse(explicit)
	}
	return repository.Current()
}

func defaultBranch(ctx context.Context, client *api.RESTClient, owner, repo string) (string, error) {
	var resp struct {
		DefaultBranch string `json:"default_branch"`
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	err := client.DoWithContext(ctx, "GET", path, nil, &resp)
	if err != nil {
		return "", err
	}
	if resp.DefaultBranch == "" {
		return "", fmt.Errorf("empty default_branch")
	}
	return resp.DefaultBranch, nil
}

// latestRunWebURL prefers the Actions API html_url; otherwise builds the canonical run page URL.
func latestRunWebURL(host, owner, repo string, runID int64, htmlURLFromAPI string) string {
	if s := strings.TrimSpace(htmlURLFromAPI); s != "" {
		return s
	}
	if host == "" || owner == "" || repo == "" || runID == 0 {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/%s/actions/runs/%d", host, owner, repo, runID)
}

type workflowRun struct {
	ID         int64   `json:"id"`
	HtmlURL    string  `json:"html_url"`
	Conclusion *string `json:"conclusion"`
	UpdatedAt  string  `json:"updated_at"`
}

func listWorkflowRuns(ctx context.Context, client *api.RESTClient, owner, repo, workflow, branch string, limit int) ([]workflowRun, error) {
	wf := url.PathEscape(workflow)
	q := url.Values{}
	q.Set("branch", branch)
	q.Set("per_page", fmt.Sprintf("%d", limit))
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/runs?%s",
		url.PathEscape(owner), url.PathEscape(repo), wf, q.Encode())

	var resp struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	runs := resp.WorkflowRuns
	// API returns newest-first; display oldest-first (left → right).
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs, nil
}

func countJobs(ctx context.Context, client *api.RESTClient, owner, repo string, runID int64, conclusion *string) (passed, total int, err error) {
	ownerE, repoE := url.PathEscape(owner), url.PathEscape(repo)
	base := fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs", ownerE, repoE, runID)

	if conclusion != nil && *conclusion == "success" {
		var resp struct {
			TotalCount int `json:"total_count"`
		}
		path := base + "?filter=latest&per_page=1"
		if err := client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
			return 0, 0, err
		}
		return resp.TotalCount, resp.TotalCount, nil
	}

	page := 1
	perPage := 100
	passed = 0
	total = 0
	for {
		var resp struct {
			TotalCount int `json:"total_count"`
			Jobs       []struct {
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		path := fmt.Sprintf("%s?filter=latest&per_page=%d&page=%d", base, perPage, page)
		if err := client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
			return 0, 0, err
		}
		if page == 1 {
			total = resp.TotalCount
		}
		if len(resp.Jobs) == 0 {
			break
		}
		for _, j := range resp.Jobs {
			if j.Conclusion == "success" {
				passed++
			}
		}
		if len(resp.Jobs) < perPage {
			break
		}
		page++
	}
	return passed, total, nil
}

func iconForConclusion(conclusion *string) string {
	if conclusion == nil || *conclusion == "" {
		return iconRunning
	}
	switch *conclusion {
	case "success":
		return iconSuccess
	case "failure", "timed_out":
		return iconFailure
	case "cancelled":
		return iconCancelled
	case "skipped", "neutral", "action_required", "stale":
		return iconCancelled
	default:
		return iconCancelled
	}
}

func formatSummary(conclusion *string, passed, total int, age string) string {
	c := ""
	if conclusion != nil {
		c = *conclusion
	}
	switch c {
	case "success":
		return fmt.Sprintf("%d/%d passed (%s)", passed, total, age)
	case "":
		if total > 0 {
			return fmt.Sprintf("%d/%d running (%s)", passed, total, age)
		}
		return fmt.Sprintf("running (%s)", age)
	case "cancelled":
		if total > 0 {
			return fmt.Sprintf("%d/%d cancelled (%s)", total-passed, total, age)
		}
		return fmt.Sprintf("cancelled (%s)", age)
	default:
		if total > 0 {
			return fmt.Sprintf("%d/%d failed (%s)", total-passed, total, age)
		}
		return fmt.Sprintf("%s (%s)", c, age)
	}
}

func timeAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	now := time.Now().UTC()
	ts := t.UTC()
	if ts.After(now) {
		return "just now"
	}
	d := now.Sub(ts)
	hours := int(d / time.Hour)
	minutes := int(d/time.Minute) % 60
	if hours > 48 {
		return fmt.Sprintf("%dd ago", hours/24)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh ago", hours)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm ago", minutes)
	}
	return "just now"
}

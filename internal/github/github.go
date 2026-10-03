// Package github is the small slice of the GitHub API revq needs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Token string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: %d: %s", e.Status, e.Body) }

type PR struct {
	Number         int
	Title          string
	Body           string
	URL            string
	Draft          bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Additions      int
	Deletions      int
	ChangedFiles   int
	BaseRef        string
	HeadSHA        string
	Author         string
	AuthorAssoc    string
	IsBot          bool
	ReviewDecision string
	Labels         []string
	CI             string
}

type File struct {
	Path      string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type Review struct {
	CommitID string          `json:"commit_id,omitempty"`
	Body     string          `json:"body"`
	Event    string          `json:"event"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

func (c *Client) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		msg := string(data)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &APIError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

const api = "https://api.github.com"

const prFields = `number title body url isDraft createdAt updatedAt
        additions deletions changedFiles baseRefName headRefOid
        authorAssociation author{login __typename}
        reviewDecision
        labels(first:20){nodes{name}}
        commits(last:1){nodes{commit{statusCheckRollup{state}}}}`

const openPRsQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    pullRequests(states:OPEN,first:50,after:$cursor,orderBy:{field:UPDATED_AT,direction:DESC}){
      pageInfo{hasNextPage endCursor}
      nodes{` + prFields + `}
    }
  }
}`

const searchQuery = `query($q:String!,$cursor:String){
  search(query:$q,type:ISSUE,first:50,after:$cursor){
    pageInfo{hasNextPage endCursor}
    nodes{... on PullRequest{` + prFields + `}}
  }
}`

type prNode struct {
	Number            int
	Title, Body, URL  string
	IsDraft           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Additions         int
	Deletions         int
	ChangedFiles      int
	BaseRefName       string
	HeadRefOid        string
	AuthorAssociation string
	Author            *struct {
		Login    string
		Typename string `json:"__typename"`
	}
	ReviewDecision string
	Labels         struct{ Nodes []struct{ Name string } }
	Commits        struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct{ State string }
			}
		}
	}
}

func (n prNode) pr() PR {
	p := PR{
		Number: n.Number, Title: n.Title, Body: n.Body, URL: n.URL, Draft: n.IsDraft,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Additions: n.Additions,
		Deletions: n.Deletions, ChangedFiles: n.ChangedFiles, BaseRef: n.BaseRefName,
		HeadSHA: n.HeadRefOid, AuthorAssoc: n.AuthorAssociation, ReviewDecision: n.ReviewDecision,
	}
	if n.Author != nil {
		p.Author = n.Author.Login
		p.IsBot = n.Author.Typename == "Bot" || strings.HasSuffix(n.Author.Login, "[bot]")
	}
	for _, l := range n.Labels.Nodes {
		p.Labels = append(p.Labels, l.Name)
	}
	if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		switch n.Commits.Nodes[0].Commit.StatusCheckRollup.State {
		case "SUCCESS":
			p.CI = "success"
		case "FAILURE", "ERROR":
			p.CI = "failure"
		default:
			p.CI = "pending"
		}
	}
	return p
}

type prPage struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []prNode
}

type gqlErrors []struct{ Message string }

func (e gqlErrors) err() error {
	if len(e) > 0 {
		return fmt.Errorf("github graphql: %s", e[0].Message)
	}
	return nil
}

// OpenPRs lists open PRs updated after since, newest first. One GraphQL call
// per 50 PRs covers CI state and review decision, so there is no per-PR fan-out.
func (c *Client) OpenPRs(ctx context.Context, owner, name string, since time.Time) ([]PR, error) {
	var out []PR
	var cursor *string
	for page := 0; page < 20; page++ {
		var resp struct {
			Data struct {
				Repository *struct{ PullRequests prPage }
			}
			Errors gqlErrors
		}
		req := map[string]any{"query": openPRsQuery, "variables": map[string]any{"owner": owner, "name": name, "cursor": cursor}}
		if err := c.do(ctx, "POST", api+"/graphql", req, &resp); err != nil {
			return nil, err
		}
		if err := resp.Errors.err(); err != nil {
			return nil, err
		}
		if resp.Data.Repository == nil {
			return nil, fmt.Errorf("github: repository %s/%s not found", owner, name)
		}
		prs := resp.Data.Repository.PullRequests
		for _, n := range prs.Nodes {
			if n.UpdatedAt.Before(since) {
				return out, nil
			}
			out = append(out, n.pr())
		}
		if !prs.PageInfo.HasNextPage {
			break
		}
		cursor = &prs.PageInfo.EndCursor
	}
	return out, nil
}

// SearchPRs runs a GitHub search query (e.g. "repo:o/r is:pr is:open
// review-requested:@me") and returns the matching PRs. @me resolves to the
// token's owner, and review-requested also matches requests to their teams.
func (c *Client) SearchPRs(ctx context.Context, query string) ([]PR, error) {
	var out []PR
	var cursor *string
	for page := 0; page < 20; page++ {
		var resp struct {
			Data   struct{ Search prPage }
			Errors gqlErrors
		}
		req := map[string]any{"query": searchQuery, "variables": map[string]any{"q": query, "cursor": cursor}}
		if err := c.do(ctx, "POST", api+"/graphql", req, &resp); err != nil {
			return nil, err
		}
		if err := resp.Errors.err(); err != nil {
			return nil, err
		}
		for _, n := range resp.Data.Search.Nodes {
			if n.Number != 0 {
				out = append(out, n.pr())
			}
		}
		if !resp.Data.Search.PageInfo.HasNextPage {
			break
		}
		cursor = &resp.Data.Search.PageInfo.EndCursor
	}
	return out, nil
}

// PRState returns open, merged or closed.
func (c *Client) PRState(ctx context.Context, owner, name string, number int) (string, error) {
	var resp struct {
		State  string
		Merged bool
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/%s/pulls/%d", api, owner, name, number), nil, &resp); err != nil {
		return "", err
	}
	if resp.Merged {
		return "merged", nil
	}
	return resp.State, nil
}

// Files returns the changed files with their patches (GitHub caps this at 3000).
func (c *Client) Files(ctx context.Context, owner, name string, number int) ([]File, error) {
	var out []File
	for page := 1; page <= 30; page++ {
		var files []File
		url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=100&page=%d", api, owner, name, number, page)
		if err := c.do(ctx, "GET", url, nil, &files); err != nil {
			return nil, err
		}
		out = append(out, files...)
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) RepoLabels(ctx context.Context, owner, name string) ([]string, error) {
	var out []string
	for page := 1; page <= 3; page++ {
		var labels []struct{ Name string }
		url := fmt.Sprintf("%s/repos/%s/%s/labels?per_page=100&page=%d", api, owner, name, page)
		if err := c.do(ctx, "GET", url, nil, &labels); err != nil {
			return nil, err
		}
		for _, l := range labels {
			out = append(out, l.Name)
		}
		if len(labels) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) AddLabels(ctx context.Context, owner, name string, number int, labels []string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", api, owner, name, number)
	return c.do(ctx, "POST", url, map[string]any{"labels": labels}, nil)
}

func (c *Client) PostReview(ctx context.Context, owner, name string, number int, r Review) error {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", api, owner, name, number)
	return c.do(ctx, "POST", url, r, nil)
}

// Package pipeline syncs PRs from GitHub and runs the agent over them.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/msyavuz/revq/internal/agent"
	"github.com/msyavuz/revq/internal/github"
	"github.com/msyavuz/revq/internal/notify"
	"github.com/msyavuz/revq/internal/store"
)

const autoPostFooter = "\n\n---\n_Automated first-pass review by an AI agent. A maintainer has not checked it yet._"

type Engine struct {
	St    *store.Store
	GH    *github.Client
	Agent agent.Runner
	Log   *slog.Logger

	started time.Time
	wake    chan struct{}
	pollNow chan struct{}

	mu     sync.Mutex
	labels map[int64][]string
}

func New(st *store.Store, gh *github.Client, ag agent.Runner, log *slog.Logger) *Engine {
	return &Engine{
		St: st, GH: gh, Agent: ag, Log: log,
		started: time.Now(),
		wake:    make(chan struct{}, 1),
		pollNow: make(chan struct{}, 1),
		labels:  map[int64][]string{},
	}
}

func (e *Engine) Start(ctx context.Context) {
	go e.pollLoop(ctx)
	go e.workLoop(ctx)
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// PollNow triggers a sync without waiting for the next tick.
func (e *Engine) PollNow() { poke(e.pollNow) }

// Enqueue queues a manual run.
func (e *Engine) Enqueue(prID int64, kind string) error {
	pr, err := e.St.PR(prID)
	if err != nil {
		return err
	}
	if _, err := e.St.EnqueueRun(prID, kind, pr.HeadSHA); err != nil {
		return err
	}
	e.RefreshColumn(context.Background(), prID)
	poke(e.wake)
	return nil
}

// --- sync

func (e *Engine) pollLoop(ctx context.Context) {
	for {
		e.syncAll(ctx)
		wait := time.Duration(max(e.St.Config().PollSeconds, 30)) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-e.pollNow:
		}
	}
}

func (e *Engine) syncAll(ctx context.Context) {
	repos, err := e.St.Repos()
	if err != nil {
		e.Log.Error("list repos", "err", err)
		return
	}
	for _, r := range repos {
		msg := ""
		if err := e.syncRepo(ctx, r); err != nil {
			msg = err.Error()
			e.Log.Error("sync", "repo", r.Full(), "err", err)
		}
		_ = e.St.SetRepoPolled(r.ID, msg)
	}
	poke(e.wake)
}

func (e *Engine) syncRepo(ctx context.Context, r store.Repo) error {
	cfg := e.St.Config()
	prs, requested, err := e.fetch(ctx, r, cfg)
	if err != nil {
		return err
	}
	policy := e.St.PolicyFor(r.ID)
	// Whether my review was requested on each PR before this sync, to spot the
	// moment a request goes away.
	wasRequested := map[int]bool{}
	if before, err := e.St.OpenPRs(r.ID); err == nil {
		for _, p := range before {
			wasRequested[p.Number] = p.Requested == 1
		}
	}
	seen := map[int]bool{}
	for _, g := range prs {
		seen[g.Number] = true
		id, err := e.St.UpsertPR(store.PR{
			RepoID: r.ID, Number: g.Number, Title: g.Title, Body: g.Body, Author: g.Author,
			AuthorAssoc: g.AuthorAssoc, IsBot: g.IsBot, URL: g.URL, State: "open", Draft: g.Draft,
			HeadSHA: g.HeadSHA, BaseRef: g.BaseRef, Additions: g.Additions, Deletions: g.Deletions,
			ChangedFiles: g.ChangedFiles, CI: g.CI, ReviewState: g.ReviewDecision, Labels: g.Labels,
			CreatedAt: g.CreatedAt.Unix(), UpdatedAt: g.UpdatedAt.Unix(), Requested: requested[g.Number],
			Avatar: g.Avatar, Unresolved: g.UnresolvedThreads,
		})
		if err != nil {
			return err
		}
		pr, err := e.St.PR(id)
		if err != nil {
			return err
		}
		if requestAnswered(wasRequested[g.Number], pr.Requested) {
			// I answered on GitHub directly, so an unposted draft is moot.
			if err := e.St.SettleDrafts(id); err != nil {
				return err
			}
		}
		if e.wantsAutoReview(pr, policy, cfg) {
			if _, err := e.St.EnqueueRun(id, "review", pr.HeadSHA); err != nil {
				return err
			}
		}
		e.RefreshColumn(ctx, id)
	}

	// Open in our DB but missing from the listing: merged, closed, or gone stale.
	known, err := e.St.OpenPRs(r.ID)
	if err != nil {
		return err
	}
	checked := 0
	for _, pr := range known {
		if seen[pr.Number] || checked >= 30 {
			continue
		}
		checked++
		state, err := e.GH.PRState(ctx, r.Owner, r.Name, pr.Number)
		if err != nil {
			return err
		}
		if state == "open" {
			state = "stale"
		}
		if err := e.St.SetPRState(pr.ID, state); err != nil {
			return err
		}
		e.RefreshColumn(ctx, pr.ID)
	}
	return nil
}

// requestAnswered reports whether a review request was just answered outside
// revq: it was requested at the last sync and no longer is. A PR that simply
// has no request (one you already reviewed and are drafting a follow-up for)
// doesn't count, so its drafts are left alone.
func requestAnswered(wasRequested bool, requestedNow int) bool {
	return wasRequested && requestedNow == 0
}

// fetch returns the PRs a repo tracks and, per PR number, whether my review is
// requested (1), not requested (0), or unknown because the repo tracks everything (-1).
func (e *Engine) fetch(ctx context.Context, r store.Repo, cfg store.Config) ([]github.PR, map[int]int, error) {
	requested := map[int]int{}
	if r.Scope == "all" {
		since := time.Now().AddDate(0, 0, -max(cfg.StaleDays, 1))
		prs, err := e.GH.OpenPRs(ctx, r.Owner, r.Name, since)
		for _, p := range prs {
			requested[p.Number] = -1
		}
		return prs, requested, err
	}
	base := fmt.Sprintf("repo:%s/%s is:pr is:open sort:updated-desc ", r.Owner, r.Name)
	prs, err := e.GH.SearchPRs(ctx, base+"review-requested:@me")
	if err != nil {
		return nil, nil, err
	}
	for _, p := range prs {
		requested[p.Number] = 1
	}
	// Ones I already reviewed stay on the board so I can see them come back.
	reviewed, err := e.GH.SearchPRs(ctx, base+"reviewed-by:@me")
	if err != nil {
		return nil, nil, err
	}
	for _, p := range reviewed {
		if _, ok := requested[p.Number]; !ok {
			requested[p.Number] = 0
			prs = append(prs, p)
		}
	}
	return prs, requested, nil
}

// Notifiers builds the notification channels a config describes.
func Notifiers(cfg store.Config) notify.Multi {
	return notify.Multi{
		notify.Webhook{URL: cfg.WebhookURL},
		notify.Telegram{Token: cfg.TelegramToken, ChatID: cfg.TelegramChatID},
	}
}

// Notify sends an event to every configured channel.
func (e *Engine) Notify(ctx context.Context, ev notify.Event) error {
	return Notifiers(e.St.Config()).Notify(ctx, ev)
}

// RefreshColumn recomputes a PR's column and notifies when it lands on the maintainer.
func (e *Engine) RefreshColumn(ctx context.Context, prID int64) {
	pr, err := e.St.PR(prID)
	if err != nil {
		return
	}
	drafts, _ := e.St.PendingDrafts()
	_, pending := drafts[prID]
	col := Column(pr, pending)
	if col == pr.Col {
		return
	}
	_ = e.St.SetCol(prID, col)
	if col != ColYou {
		return
	}
	// Don't flood on first import: only brand-new PRs notify when first seen.
	if pr.Col == "" && pr.CreatedAt < e.started.Unix() {
		return
	}
	reason := "ready for your review"
	switch {
	case pending:
		reason = "draft review waiting for approval"
	case pr.Assessment != nil && pr.Assessment.NeedsMaintainer:
		reason = "maintainer decision needed"
	}
	if err := e.Notify(ctx, notify.Event{Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, Reason: reason}); err != nil {
		e.Log.Warn("notify", "err", err)
	}
}

// --- worker

func startOfDay() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

// OverBudget reports whether today's spend has hit the daily cap.
func (e *Engine) OverBudget() bool {
	cfg := e.St.Config()
	return cfg.DailyBudgetUSD > 0 && e.St.SpendSince(startOfDay()) >= cfg.DailyBudgetUSD
}

func (e *Engine) workLoop(ctx context.Context) {
	for {
		var run store.Run
		ok := false
		if !e.OverBudget() {
			var err error
			run, ok, err = e.St.NextRun()
			if err != nil {
				e.Log.Error("next run", "err", err)
			}
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-time.After(time.Minute):
			}
			continue
		}
		e.RefreshColumn(ctx, run.PRID)
		var err error
		switch run.Kind {
		case "review":
			err = e.review(ctx, &run)
		default:
			err = fmt.Errorf("unknown run kind %q", run.Kind)
		}
		run.Status = "done"
		if err != nil {
			run.Status, run.Error = "failed", err.Error()
			e.Log.Error("run failed", "kind", run.Kind, "pr", run.PRID, "err", err)
		} else {
			e.Log.Info("run done", "kind", run.Kind, "pr", run.PRID, "cost", run.CostUSD, "in", run.InputTokens, "out", run.OutputTokens)
		}
		if err := e.St.FinishRun(run); err != nil {
			e.Log.Error("finish run", "err", err)
		}
		e.RefreshColumn(ctx, run.PRID)
		if ctx.Err() != nil {
			return
		}
	}
}

func (e *Engine) call(ctx context.Context, run *store.Run, req agent.Request, out any) error {
	res, err := e.Agent.Run(ctx, req)
	run.CostUSD += res.CostUSD
	run.InputTokens += res.InputTokens
	run.OutputTokens += res.OutputTokens
	if err != nil {
		return err
	}
	return json.Unmarshal(res.Output, out)
}

func (e *Engine) load(ctx context.Context, run *store.Run) (store.PR, []github.File, map[string]bool, error) {
	pr, err := e.St.PR(run.PRID)
	if err != nil {
		return pr, nil, nil, err
	}
	run.HeadSHA = pr.HeadSHA
	owner, name, _ := store.SplitRepo(pr.Repo)
	files, err := e.GH.Files(ctx, owner, name, pr.Number)
	if err != nil {
		return pr, nil, nil, err
	}
	globs := parseGlobs(e.St.Config().IgnoreGlobs)
	noise := map[string]bool{}
	for _, f := range files {
		if isNoise(f.Path, globs) {
			noise[f.Path] = true
		}
	}
	return pr, files, noise, nil
}

func (e *Engine) repoLabels(ctx context.Context, pr store.PR) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := e.labels[pr.RepoID]; ok {
		return l
	}
	owner, name, _ := store.SplitRepo(pr.Repo)
	l, err := e.GH.RepoLabels(ctx, owner, name)
	if err != nil {
		e.Log.Warn("labels", "repo", pr.Repo, "err", err)
		return nil
	}
	e.labels[pr.RepoID] = l
	return l
}

// newLabels keeps suggestions that exist in the repo and aren't on the PR yet.
func newLabels(suggested, repo, have []string) []string {
	ok := map[string]bool{}
	for _, l := range repo {
		ok[l] = true
	}
	for _, l := range have {
		ok[l] = false
	}
	var out []string
	for _, l := range suggested {
		if ok[l] {
			out = append(out, l)
			ok[l] = false
		}
	}
	return out
}

// AutoReviewOn reports whether auto review applies to a PR. The PR's own
// switch wins if the maintainer set one; otherwise it is on when the
// repository's policy says so or the author is on the trusted list.
func AutoReviewOn(pr store.PR, policy store.Policy, cfg store.Config) bool {
	if pr.AutoReview >= 0 {
		return pr.AutoReview == 1
	}
	return policy.AutoReview || cfg.Trusts(pr.Author)
}

// SetAutoReview records the per-PR choice and, when it turns auto review on,
// queues a review straight away instead of waiting for the next sync.
func (e *Engine) SetAutoReview(prID int64, v int) error {
	if err := e.St.SetAutoReview(prID, v); err != nil {
		return err
	}
	pr, err := e.St.PR(prID)
	if err != nil {
		return err
	}
	if pr.State == "open" && e.wantsAutoReview(pr, e.St.PolicyFor(pr.RepoID), e.St.Config()) {
		return e.Enqueue(prID, "review")
	}
	return nil
}

// wantsAutoReview decides whether a PR gets an unattended review. Each commit
// is reviewed at most once, and pushes don't re-burn tokens while a draft is
// still waiting: review once, then again only after the previous review was
// posted (or answered on GitHub) and the author has pushed.
func (e *Engine) wantsAutoReview(pr store.PR, policy store.Policy, cfg store.Config) bool {
	if !AutoReviewOn(pr, policy, cfg) {
		return false
	}
	// Requested == 0 means the ball is with the author; no tokens until they ask again.
	if pr.Requested == 0 {
		return false
	}
	// The bot and draft filters are for the blanket policy. Turning auto review
	// on for one PR by hand says "this one", whatever kind it is.
	if pr.AutoReview != 1 && ((pr.IsBot && !cfg.ReviewBots) || (pr.Draft && !cfg.ReviewDrafts)) {
		return false
	}
	if e.St.HasRun(pr.ID, "review", pr.HeadSHA) {
		return false
	}
	d, ok := e.St.LatestDraft(pr.ID)
	if !ok {
		return true
	}
	return d.Status == "posted" && d.HeadSHA != pr.HeadSHA
}

// looksReady reports whether a review amounts to "nothing stands in the way of
// merging, as far as the diff shows": an approving verdict, no findings, and
// no part of the change left unread.
func looksReady(verdict string, findings, skipped int) bool {
	return verdict == "APPROVE" && findings == 0 && skipped == 0
}

var verdictRank = map[string]int{"APPROVE": 0, "COMMENT": 1, "REQUEST_CHANGES": 2}

var riskRank = map[string]int{"low": 0, "medium": 1, "high": 2}

func (e *Engine) review(ctx context.Context, run *store.Run) error {
	cfg := e.St.Config()
	run.Model = cfg.ReviewModel
	pr, files, noise, err := e.load(ctx, run)
	if err != nil {
		return err
	}
	plan := planReview(files, noise, max(cfg.ReviewChunkChars, 4000), max(cfg.ReviewMaxChunks, 1))
	if len(plan.Chunks) == 0 {
		return errors.New("nothing reviewable in this PR (only generated files or no diff)")
	}
	policy := e.St.PolicyFor(pr.RepoID)
	var labels []string
	if policy.AutoLabel {
		labels = e.repoLabels(ctx, pr)
	}
	// A partial view needs the whole file list to know what it isn't seeing.
	overview := ""
	if len(plan.Chunks) > 1 || len(plan.Skipped) > 0 {
		overview = fileList(files, noise, 150)
	}

	draft := store.Draft{PRID: pr.ID, HeadSHA: pr.HeadSHA, Verdict: "APPROVE"}
	read := store.Assessment{Risk: "low"}
	var summaries []string
	for i, chunk := range plan.Chunks {
		var out struct {
			Summary             string          `json:"summary"`
			Risk                string          `json:"risk"`
			Verdict             string          `json:"verdict"`
			Findings            []store.Finding `json:"findings"`
			MaintainerQuestions []string        `json:"maintainer_questions"`
			SuggestedLabels     []string        `json:"suggested_labels"`
			ReadyOverview       []string        `json:"ready_overview"`
		}
		err := e.call(ctx, run, agent.Request{
			System:    reviewSystem,
			Prompt:    reviewPrompt(pr, chunk, overview, i+1, len(plan.Chunks), cfg.Guidelines, labels),
			Model:     cfg.ReviewModel,
			Schema:    reviewSchema,
			BudgetUSD: cfg.RunBudgetUSD,
		}, &out)
		if err != nil {
			return fmt.Errorf("chunk %d/%d: %w", i+1, len(plan.Chunks), err)
		}
		summaries = append(summaries, out.Summary)
		if verdictRank[out.Verdict] > verdictRank[draft.Verdict] {
			draft.Verdict = out.Verdict
		}
		if riskRank[out.Risk] > riskRank[read.Risk] {
			read.Risk = out.Risk
		}
		read.MaintainerQuestions = append(read.MaintainerQuestions, out.MaintainerQuestions...)
		read.SuggestedLabels = append(read.SuggestedLabels, out.SuggestedLabels...)
		read.ReadyOverview = append(read.ReadyOverview, out.ReadyOverview...)
		for _, f := range out.Findings {
			f.Inline = plan.Valid[f.Path][f.Line]
			f.Snippet = snippet(plan.Patch[f.Path], f.Line, 4, 1)
			draft.Findings = append(draft.Findings, f)
		}
	}
	// An approval that didn't see everything isn't an approval.
	if len(plan.Skipped) > 0 && draft.Verdict == "APPROVE" {
		draft.Verdict = "COMMENT"
	}
	// The review is its inline comments; the overall comment starts empty and
	// is the maintainer's to write.
	if n := len(plan.Skipped); n > 0 {
		draft.Notes = fmt.Sprintf("Partial review: the agent read %d of %d changed files. The other %d were over the review size budget or had no diff on GitHub.",
			len(plan.Valid), len(plan.Valid)+n, n)
	}

	// The overview is only a merge brief when the whole change was read and
	// nothing was found; anything less and it would overstate what was checked.
	read.Ready = looksReady(draft.Verdict, len(draft.Findings), len(plan.Skipped))
	if !read.Ready {
		read.ReadyOverview = nil
	}
	read.Summary = summaries[0]
	read.NeedsMaintainer = len(read.MaintainerQuestions) > 0
	if err := e.St.SetAssessment(pr.ID, read, pr.HeadSHA); err != nil {
		return err
	}
	if policy.AutoLabel {
		if add := newLabels(read.SuggestedLabels, labels, pr.Labels); len(add) > 0 {
			owner, name, _ := store.SplitRepo(pr.Repo)
			if err := e.GH.AddLabels(ctx, owner, name, pr.Number, add); err != nil {
				e.Log.Warn("add labels", "pr", pr.Number, "err", err)
			}
		}
	}

	id, err := e.St.CreateDraft(draft)
	if err != nil {
		return err
	}
	if policy.AutoPost {
		d, err := e.St.Draft(id)
		if err != nil {
			return err
		}
		// Unattended posts never approve or block; that stays a human call.
		if len(d.Findings) == 0 {
			return nil // nothing to say; leave the draft for the maintainer
		}
		if err := e.PostDraft(ctx, d, "COMMENT", strings.TrimSpace(autoPostFooter), d.Findings); err != nil {
			return fmt.Errorf("auto-post: %w", err)
		}
	}
	return nil
}

// Severity is for the maintainer's eyes in revq. What goes to GitHub is the
// comment text alone, as the maintainer left it.
//
// PostDraft sends a review to GitHub. With an event (COMMENT, APPROVE,
// REQUEST_CHANGES) it is published. With an empty event it becomes a pending
// review that only the maintainer can see, to be finished in GitHub's own UI.
// Findings on lines GitHub can't anchor are folded into the body, not dropped.
func (e *Engine) PostDraft(ctx context.Context, d store.Draft, event, body string, findings []store.Finding) error {
	pr, err := e.St.PR(d.PRID)
	if err != nil {
		return err
	}
	owner, name, _ := store.SplitRepo(pr.Repo)
	build := func(inline bool) github.Review {
		r := github.Review{CommitID: d.HeadSHA, Event: event}
		var extra []string
		for _, f := range findings {
			if inline && f.Inline {
				r.Comments = append(r.Comments, github.ReviewComment{
					Path: f.Path, Line: f.Line, Side: "RIGHT",
					Body: f.Body,
				})
			} else {
				extra = append(extra, fmt.Sprintf("- `%s:%d` %s", f.Path, f.Line, f.Body))
			}
		}
		r.Body = strings.TrimSpace(body)
		if len(extra) > 0 {
			r.Body = strings.TrimSpace(r.Body + "\n\n" + strings.Join(extra, "\n"))
		}
		return r
	}
	review := build(true)
	err = e.GH.PostReview(ctx, owner, name, pr.Number, review)
	var apiErr *github.APIError
	if errors.As(err, &apiErr) && apiErr.Status == 422 && len(review.Comments) > 0 {
		// Usually a line that no longer resolves after a push.
		err = e.GH.PostReview(ctx, owner, name, pr.Number, build(false))
	}
	if err != nil {
		return err
	}
	if event == "" {
		// Not published yet, so the card stays with the maintainer.
		return e.St.SetDraftStatus(d.ID, "pending")
	}
	if err := e.St.SetDraftStatus(d.ID, "posted"); err != nil {
		return err
	}
	if err := e.St.SetPostedSHA(pr.ID, d.HeadSHA); err != nil {
		return err
	}
	e.RefreshColumn(ctx, pr.ID)
	return nil
}

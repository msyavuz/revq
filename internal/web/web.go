// Package web serves the kanban board and the review approval UI.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/msyavuz/revq/internal/notify"
	"github.com/msyavuz/revq/internal/pipeline"
	"github.com/msyavuz/revq/internal/store"
	"github.com/msyavuz/revq/internal/update"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	st      *store.Store
	eng     *pipeline.Engine
	log     *slog.Logger
	logins  *limiter
	version string
	latest  atomic.Value // newest release tag, checked in the background
	tpl     map[string]*template.Template
}

func New(st *store.Store, eng *pipeline.Engine, log *slog.Logger, version string) http.Handler {
	s := &Server{st: st, eng: eng, log: log, version: version, logins: &limiter{fails: map[string][]time.Time{}}, tpl: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"ago":      ago,
		"initial":  initial,
		"markdown": renderMarkdown,
		"repoName": func(full string) string { return full[strings.LastIndex(full, "/")+1:] },
		"addShare": addShare,
		"money":    func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"cents":    func(v float64) string { return fmt.Sprintf("$%.3f", v) },
		"short": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
	}
	for _, name := range []string{"board", "pr", "settings", "account"} {
		s.tpl[name] = template.Must(template.New("").Funcs(funcs).
			ParseFS(assets, "templates/layout.html", "templates/"+name+".html"))
	}

	s.tpl["login"] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/login.html"))

	go s.watchReleases()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /account", s.account)
	mux.HandleFunc("POST /account", s.saveAccount)
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.board)
	mux.HandleFunc("GET /board", s.boardFragment)
	mux.HandleFunc("POST /poll", s.poll)
	mux.HandleFunc("GET /pr/{id}", s.pr)
	mux.HandleFunc("POST /pr/{id}/move", s.move)
	mux.HandleFunc("POST /pr/{id}/run", s.run)
	mux.HandleFunc("POST /pr/{id}/auto", s.setAuto)
	mux.HandleFunc("POST /pr/{id}/trust", s.setTrust)
	mux.HandleFunc("POST /draft/{id}/post", s.postDraft)
	mux.HandleFunc("POST /draft/{id}/discard", s.discardDraft)
	mux.HandleFunc("POST /draft/{id}/finished", s.finishedDraft)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /settings/test-notify", s.testNotify)
	mux.HandleFunc("POST /settings/telegram-token/remove", s.removeTelegramToken)
	mux.HandleFunc("POST /repos", s.addRepo)
	mux.HandleFunc("POST /repos/{id}/policy", s.repoPolicy)
	mux.HandleFunc("POST /repos/{id}/delete", s.deleteRepo)
	return s.guard(mux)
}

func ago(ts int64) string {
	if ts == 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

type page struct {
	Title      string
	Flash      string
	Spend      float64
	Budget     float64
	Queued     int
	OverBudget bool
	Version    string
	Update     string // a newer release, if there is one
	Data       any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	p := page{
		Title: title, Flash: r.URL.Query().Get("msg"), Data: data, Version: s.version, Update: s.updateAvailable(),
		Spend: s.st.SpendSince(midnight), Budget: s.st.Config().DailyBudgetUSD,
		Queued: s.st.QueuedCount(), OverBudget: s.eng.OverBudget(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("render", "tpl", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("handler", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func back(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		to += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// --- board

type card struct {
	PR         store.PR
	Risk       string
	Running    string
	HasDraft   bool
	DraftStale bool
	Decision   bool
	Large      bool
	Pinned     bool
	Reviewed   bool // a review was already posted for this exact commit
	Ready      bool // the agent read the whole change at this commit and found nothing
	AutoOn     bool // auto review was switched on for this PR specifically
	AutoOff    bool // auto review was switched off for this PR specifically
}

type column struct {
	Key, Title, Hint, Empty string
	Cards                   []card
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// renderMarkdown turns a PR description into HTML. goldmark drops raw HTML and
// unsafe link schemes unless told otherwise, which is what makes it safe to
// mark the result as trusted: the input is whatever a PR author typed.
func renderMarkdown(src string) template.HTML {
	var b strings.Builder
	if err := md.Convert([]byte(src), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(b.String())
}

// shortNote collapses the file list that older drafts stored in their note
// into the one-line count newer drafts carry.
func shortNote(note string) string {
	list, ok := strings.CutPrefix(note, "Not reviewed (over the size budget or no diff): ")
	if !ok {
		return note
	}
	n := strings.Count(list, ", ") + 1
	return fmt.Sprintf("Partial review: %d changed files were over the review size budget or had no diff on GitHub, so the agent didn't read them.", n)
}

// watchReleases looks for a newer release now and then, so the footer can
// say when one is out. It only reads GitHub's public release list.
func (s *Server) watchReleases() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if tag, err := update.Latest(ctx); err == nil {
			s.latest.Store(tag)
		}
		cancel()
		time.Sleep(12 * time.Hour)
	}
}

func (s *Server) updateAvailable() string {
	if tag, _ := s.latest.Load().(string); update.Newer(tag, s.version) {
		return tag
	}
	return ""
}

// readyNow reports whether the agent's "looks ready" verdict still applies:
// it was reached for the commit the PR is on now.
func readyNow(pr store.PR) bool {
	a := pr.Assessment
	return a != nil && a.Ready && pr.State == "open" && pr.AssessedSHA == pr.HeadSHA
}

// initial is the fallback shown when an author has no profile picture.
func initial(login string) string {
	if login == "" {
		return "?"
	}
	return strings.ToUpper(login[:1])
}

// addShare is the percentage of changed lines that are additions.
func addShare(add, del int) int {
	if add+del == 0 {
		return 100
	}
	return add * 100 / (add + del)
}

// largeChange is the changed-line count from which a card is flagged as large.
const largeChange = 1500

var riskRank = map[string]int{"high": 0, "medium": 1, "low": 2, "": 3}

func (s *Server) columns() ([]column, error) {
	prs, err := s.st.BoardPRs()
	if err != nil {
		return nil, err
	}
	active, err := s.st.ActiveRuns()
	if err != nil {
		return nil, err
	}
	drafts, err := s.st.PendingDrafts()
	if err != nil {
		return nil, err
	}
	// Whether each repository auto reviews by default, to tell an exception from the rule.
	cfg := s.st.Config()
	policyOn := map[int64]bool{}
	if repos, err := s.st.Repos(); err == nil {
		for _, r := range repos {
			policyOn[r.ID] = s.st.PolicyFor(r.ID).AutoReview
		}
	}
	byCol := map[string][]card{}
	for _, pr := range prs {
		c := card{PR: pr, Running: active[pr.ID], Large: pr.Additions+pr.Deletions >= largeChange}
		if a := pr.Assessment; a != nil {
			c.Risk = a.Risk
			c.Decision = a.NeedsMaintainer
		}
		if sha, ok := drafts[pr.ID]; ok {
			c.HasDraft, c.DraftStale = true, sha != pr.HeadSHA
		}
		c.Ready = readyNow(pr)
		c.Reviewed = pr.PostedSHA != "" && pr.PostedSHA == pr.HeadSHA
		// Only mark exceptions: on where the default is off, off where it is on.
		byDefault := policyOn[pr.RepoID] || cfg.Trusts(pr.Author)
		c.AutoOn, c.AutoOff = pr.AutoReview == 1 && !byDefault, pr.AutoReview == 0 && byDefault
		c.Pinned = pr.State == "open" && pr.ColOverride != "" && pr.OverrideSHA == pr.HeadSHA
		col := pr.Col
		if !pipeline.ValidColumn(col) {
			col = pipeline.ColYou
		}
		byCol[col] = append(byCol[col], c)
	}
	// Needs-you is a work queue: riskiest first. BoardPRs already sorts by recency.
	you := byCol[pipeline.ColYou]
	sort.SliceStable(you, func(i, j int) bool { return riskRank[you[i].Risk] < riskRank[you[j].Risk] })
	if done := byCol[pipeline.ColDone]; len(done) > 25 {
		byCol[pipeline.ColDone] = done[:25]
	}
	var out []column
	for _, c := range pipeline.Columns {
		out = append(out, column{Key: c.Key, Title: c.Title, Hint: c.Hint, Empty: c.Empty, Cards: byCol[c.Key]})
	}
	return out, nil
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	cols, err := s.columns()
	if err != nil {
		s.fail(w, err)
		return
	}
	repos, _ := s.st.Repos()
	s.render(w, r, "board", "Board", map[string]any{"Columns": cols, "NoRepos": len(repos) == 0})
}

func (s *Server) boardFragment(w http.ResponseWriter, r *http.Request) {
	cols, err := s.columns()
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl["board"].ExecuteTemplate(w, "columns", cols); err != nil {
		s.log.Error("render", "err", err)
	}
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	s.eng.PollNow()
	back(w, r, "/", "Sync started")
}

// --- pr

func (s *Server) pr(w http.ResponseWriter, r *http.Request) {
	pr, err := s.st.PR(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	runs, err := s.st.RunsForPR(pr.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := map[string]any{
		"PR": pr, "Runs": runs,
		"Pinned": pr.ColOverride != "" && pr.OverrideSHA == pr.HeadSHA,
	}
	active, _ := s.st.ActiveRuns()
	data["Running"] = active[pr.ID]
	data["Ready"] = readyNow(pr)
	policy := s.st.PolicyFor(pr.RepoID)
	cfg := s.st.Config()
	data["AutoOn"] = pipeline.AutoReviewOn(pr, policy, cfg)
	data["AutoSet"] = pr.AutoReview >= 0
	data["Trusted"] = cfg.Trusts(pr.Author)
	// What the switch falls back to when this PR has no setting of its own.
	data["RepoAuto"] = policy.AutoReview || cfg.Trusts(pr.Author)
	// What the review button should say depends on what already exists.
	data["ReviewLabel"] = "Draft review"
	if d, ok := s.st.LatestDraft(pr.ID); ok {
		stale := d.HeadSHA != pr.HeadSHA
		d.Notes = shortNote(d.Notes)
		data["Draft"] = d
		data["DraftStale"] = stale
		switch {
		case d.Status == "pending":
			data["ReviewLabel"] = ""
		case d.Status == "draft" && stale:
			data["ReviewLabel"] = "Redraft for the new commits"
			data["ReplacesDraft"] = true
		case d.Status == "draft":
			data["ReviewLabel"] = "Redraft review"
			data["ReplacesDraft"] = true
		case stale:
			data["ReviewLabel"] = "Draft review of the new commits"
		default:
			data["ReviewLabel"] = "Draft another review"
		}
	}
	s.render(w, r, "pr", fmt.Sprintf("%s#%d", pr.Repo, pr.Number), data)
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	pr, err := s.st.PR(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	col := r.FormValue("col")
	switch {
	case col == "auto":
		err = s.st.SetOverride(id, "", "")
	case pr.State == "open" && pipeline.ValidColumn(col) && col != pipeline.ColDone:
		err = s.st.SetOverride(id, col, pr.HeadSHA)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.eng.RefreshColumn(r.Context(), id)
	if r.Header.Get("HX-Request") != "" {
		s.boardFragment(w, r)
		return
	}
	back(w, r, fmt.Sprintf("/pr/%d", id), "")
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	kind := r.FormValue("kind")
	if kind != "review" {
		http.Error(w, "bad kind", http.StatusBadRequest)
		return
	}
	if err := s.eng.Enqueue(id, kind); err != nil {
		s.fail(w, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		s.boardFragment(w, r)
		return
	}
	msg := "Review queued"
	if s.eng.OverBudget() {
		msg += " (daily budget reached, it will wait until tomorrow or a higher budget)"
	}
	back(w, r, fmt.Sprintf("/pr/%d", id), msg)
}

// setAuto switches auto review on or off for one PR, or back to following
// its repository's setting.
func (s *Server) setAuto(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	v, msg := -1, "This PR uses the default auto review setting again"
	switch r.FormValue("auto") {
	case "on":
		v, msg = 1, "Auto review is on for this PR"
	case "off":
		v, msg = 0, "Auto review is off for this PR"
	}
	if err := s.eng.SetAutoReview(id, v); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, fmt.Sprintf("/pr/%d", id), msg)
}

// setTrust adds or removes a PR's author from the trusted list, then syncs so
// their other open PRs are picked up without waiting for the next poll.
func (s *Server) setTrust(w http.ResponseWriter, r *http.Request) {
	pr, err := s.st.PR(pathID(r))
	if err != nil || pr.Author == "" {
		http.NotFound(w, r)
		return
	}
	trusted := r.FormValue("trust") == "on"
	c := s.st.Config()
	c.SetTrust(pr.Author, trusted)
	if err := s.st.SaveConfig(c); err != nil {
		s.fail(w, err)
		return
	}
	msg := pr.Author + " is no longer a trusted author"
	if trusted {
		msg = "PRs from " + pr.Author + " are now auto reviewed"
		// This PR first; the rest follow on the sync.
		if err := s.eng.SetAutoReview(pr.ID, pr.AutoReview); err != nil {
			s.fail(w, err)
			return
		}
		s.eng.PollNow()
	}
	back(w, r, fmt.Sprintf("/pr/%d", pr.ID), msg)
}

func (s *Server) postDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Draft(pathID(r))
	if err != nil || d.Status != "draft" {
		http.Error(w, "draft not found or already handled", http.StatusNotFound)
		return
	}
	event := r.FormValue("verdict")
	if event != "APPROVE" && event != "REQUEST_CHANGES" {
		event = "COMMENT"
	}
	var keep []store.Finding
	for i, f := range d.Findings {
		if r.FormValue(fmt.Sprintf("f%d", i)) == "" {
			continue
		}
		if body := strings.TrimSpace(r.FormValue(fmt.Sprintf("fb%d", i))); body != "" {
			f.Body = body
		}
		keep = append(keep, f)
	}
	dest := fmt.Sprintf("/pr/%d", d.PRID)
	// GitHub rejects a comment-only review that says nothing.
	if strings.TrimSpace(r.FormValue("body")) == "" && len(keep) == 0 && event != "APPROVE" {
		back(w, r, dest, "Nothing to send: keep at least one finding or write an overall comment.")
		return
	}
	done := "Review posted"
	if r.FormValue("pending") != "" {
		event = ""
		done = "Sent to GitHub as a pending review. Only you can see it until you submit it there."
	}
	if err := s.eng.PostDraft(r.Context(), d, event, r.FormValue("body"), keep); err != nil {
		back(w, r, dest, "Sending to GitHub failed: "+err.Error())
		return
	}
	back(w, r, dest, done)
}

// finishedDraft records that a pending review was submitted on GitHub. revq
// notices by itself when the review request clears; this covers PRs that had
// no open request to clear.
func (s *Server) finishedDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Draft(pathID(r))
	if err != nil || d.Status != "pending" {
		http.Error(w, "no pending review to finish", http.StatusNotFound)
		return
	}
	if err := s.st.SetDraftStatus(d.ID, "posted"); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.SetPostedSHA(d.PRID, d.HeadSHA); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.RefreshColumn(r.Context(), d.PRID)
	back(w, r, fmt.Sprintf("/pr/%d", d.PRID), "Marked as submitted")
}

func (s *Server) discardDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Draft(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if d.Status == "pending" {
		back(w, r, fmt.Sprintf("/pr/%d", d.PRID), "This review is pending on GitHub. Discard it there.")
		return
	}
	if err := s.st.SetDraftStatus(d.ID, "discarded"); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.RefreshColumn(r.Context(), d.PRID)
	back(w, r, fmt.Sprintf("/pr/%d", d.PRID), "Draft discarded")
}

// --- settings

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	repos, err := s.st.Repos()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "settings", "Settings", map[string]any{"Config": s.st.Config(), "Repos": repos})
}

func formPolicy(r *http.Request) store.Policy {
	return store.Policy{
		AutoReview: r.FormValue("auto_review") != "",
		AutoLabel:  r.FormValue("auto_label") != "",
		AutoPost:   r.FormValue("auto_post") != "",
	}
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	c := s.st.Config()
	num := func(name string, def int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name))); err == nil && v > 0 {
			return v
		}
		return def
	}
	usd := func(name string, def float64) float64 {
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64); err == nil && v >= 0 {
			return v
		}
		return def
	}
	text := func(name, def string) string {
		if v := strings.TrimSpace(r.FormValue(name)); v != "" {
			return v
		}
		return def
	}
	c.Policy = formPolicy(r)
	c.ReviewModel = text("review_model", c.ReviewModel)
	c.PollSeconds = num("poll_seconds", c.PollSeconds)
	c.StaleDays = num("stale_days", c.StaleDays)
	c.DailyBudgetUSD = usd("daily_budget_usd", c.DailyBudgetUSD)
	c.RunBudgetUSD = usd("run_budget_usd", c.RunBudgetUSD)
	c.ReviewChunkChars = num("review_chunk_chars", c.ReviewChunkChars)
	c.ReviewMaxChunks = num("review_max_chunks", c.ReviewMaxChunks)
	c.ReviewBots = r.FormValue("review_bots") != ""
	c.ReviewDrafts = r.FormValue("review_drafts") != ""
	c.TrustedAuthors = r.FormValue("trusted_authors")
	c.TrustedAuthors = strings.Join(c.AuthorList(), "\n")
	c.IgnoreGlobs = strings.TrimSpace(r.FormValue("ignore_globs"))
	c.Guidelines = strings.TrimSpace(r.FormValue("guidelines"))
	c.WebhookURL = strings.TrimSpace(r.FormValue("webhook_url"))
	c.TelegramChatID = strings.TrimSpace(r.FormValue("telegram_chat_id"))
	// The token is never echoed back into the form, so blank means "keep".
	if t := strings.TrimSpace(r.FormValue("telegram_token")); t != "" {
		c.TelegramToken = t
	}
	if err := s.st.SaveConfig(c); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, "/settings", "Settings saved")
}

// testNotify tries the channels as currently typed into the form, saved or not.
func (s *Server) testNotify(w http.ResponseWriter, r *http.Request) {
	c := s.st.Config()
	c.WebhookURL = strings.TrimSpace(r.FormValue("webhook_url"))
	c.TelegramChatID = strings.TrimSpace(r.FormValue("telegram_chat_id"))
	if t := strings.TrimSpace(r.FormValue("telegram_token")); t != "" {
		c.TelegramToken = t
	}
	msg := "Sent. Check your chat."
	switch {
	case c.WebhookURL == "" && (c.TelegramToken == "" || c.TelegramChatID == ""):
		msg = "Fill in a webhook URL, or a Telegram token and chat ID, first."
	default:
		err := pipeline.Notifiers(c).Notify(r.Context(), notify.Event{
			Repo: "revq/test", Number: 1, Title: "Test notification from revq",
			URL: "https://github.com", Reason: "this is a test",
		})
		if err != nil {
			msg = "Failed: " + err.Error()
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(template.HTMLEscapeString(msg)))
}

func (s *Server) removeTelegramToken(w http.ResponseWriter, r *http.Request) {
	c := s.st.Config()
	c.TelegramToken = ""
	if err := s.st.SaveConfig(c); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl["settings"].ExecuteTemplate(w, "tgtoken", c); err != nil {
		s.log.Error("render", "err", err)
	}
}

func (s *Server) addRepo(w http.ResponseWriter, r *http.Request) {
	owner, name, ok := store.SplitRepo(r.FormValue("repo"))
	if !ok {
		back(w, r, "/settings", "Use owner/name")
		return
	}
	if err := s.st.AddRepo(owner, name, formScope(r)); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.PollNow()
	back(w, r, "/settings", "Added "+owner+"/"+name+", syncing")
}

func formScope(r *http.Request) string {
	if r.FormValue("scope") == "all" {
		return "all"
	}
	return "requested"
}

func (s *Server) repoPolicy(w http.ResponseWriter, r *http.Request) {
	if err := s.st.SetRepoScope(pathID(r), formScope(r)); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.PollNow()
	// Whatever is ticked is what gets saved. A repository only keeps settings
	// of its own when they differ from the defaults, so one that matches keeps
	// following the defaults if those change later.
	var p *store.Policy
	msg := "Saved. This repository follows the default automation settings."
	if v := formPolicy(r); r.FormValue("reset") == "" && v != s.st.Config().Policy {
		p = &v
		msg = "Saved. This repository has its own automation settings."
	}
	if err := s.st.SetRepoPolicy(pathID(r), p); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, "/settings", msg)
}

func (s *Server) deleteRepo(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteRepo(pathID(r)); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, "/settings", "Repo removed")
}

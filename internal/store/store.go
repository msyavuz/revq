// Package store is the SQLite persistence layer.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS repos (
  id INTEGER PRIMARY KEY,
  owner TEXT NOT NULL,
  name TEXT NOT NULL,
  policy TEXT NOT NULL DEFAULT '',
  last_polled INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  UNIQUE(owner, name)
);
CREATE TABLE IF NOT EXISTS prs (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number INTEGER NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL DEFAULT '',
  author_assoc TEXT NOT NULL DEFAULT '',
  is_bot INTEGER NOT NULL DEFAULT 0,
  url TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'open',
  draft INTEGER NOT NULL DEFAULT 0,
  head_sha TEXT NOT NULL DEFAULT '',
  base_ref TEXT NOT NULL DEFAULT '',
  additions INTEGER NOT NULL DEFAULT 0,
  deletions INTEGER NOT NULL DEFAULT 0,
  changed_files INTEGER NOT NULL DEFAULT 0,
  ci TEXT NOT NULL DEFAULT '',
  review_state TEXT NOT NULL DEFAULT '',
  labels TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  col TEXT NOT NULL DEFAULT '',
  col_override TEXT NOT NULL DEFAULT '',
  override_sha TEXT NOT NULL DEFAULT '',
  triage TEXT NOT NULL DEFAULT '',
  triage_sha TEXT NOT NULL DEFAULT '',
  posted_sha TEXT NOT NULL DEFAULT '',
  UNIQUE(repo_id, number)
);
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES prs(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  head_sha TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'queued',
  model TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS runs_status ON runs(status);
CREATE TABLE IF NOT EXISTS drafts (
  id INTEGER PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES prs(id) ON DELETE CASCADE,
  head_sha TEXT NOT NULL,
  verdict TEXT NOT NULL DEFAULT 'COMMENT',
  body TEXT NOT NULL DEFAULT '',
  findings TEXT NOT NULL DEFAULT '[]',
  notes TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'draft',
  created_at INTEGER NOT NULL,
  posted_at INTEGER NOT NULL DEFAULT 0
);
`

// Policy is what the agent may do without asking.
type Policy struct {
	AutoReview bool `json:"auto_review"`
	AutoLabel  bool `json:"auto_label"`
	AutoPost   bool `json:"auto_post"`
}

type Config struct {
	Policy           Policy  `json:"policy"`
	TriageModel      string  `json:"triage_model"`
	ReviewModel      string  `json:"review_model"`
	PollSeconds      int     `json:"poll_seconds"`
	StaleDays        int     `json:"stale_days"`
	DailyBudgetUSD   float64 `json:"daily_budget_usd"`
	RunBudgetUSD     float64 `json:"run_budget_usd"`
	TriageDiffChars  int     `json:"triage_diff_chars"`
	ReviewChunkChars int     `json:"review_chunk_chars"`
	ReviewMaxChunks  int     `json:"review_max_chunks"`
	ReviewBots       bool    `json:"review_bots"`
	ReviewDrafts     bool    `json:"review_drafts"`
	IgnoreGlobs      string  `json:"ignore_globs"`
	Guidelines       string  `json:"guidelines"`
	WebhookURL       string  `json:"webhook_url"`
	TelegramToken    string  `json:"telegram_token"`
	TelegramChatID   string  `json:"telegram_chat_id"`
}

func DefaultConfig() Config {
	return Config{
		TriageModel:      "haiku",
		ReviewModel:      "sonnet",
		PollSeconds:      120,
		StaleDays:        30,
		DailyBudgetUSD:   3,
		RunBudgetUSD:     1,
		TriageDiffChars:  24000,
		ReviewChunkChars: 120000,
		ReviewMaxChunks:  3,
	}
}

type Repo struct {
	ID         int64
	Owner      string
	Name       string
	Policy     *Policy // nil = inherit global
	Scope      string  // "requested": PRs waiting on my review or already reviewed by me; "all": every open PR
	LastPolled int64
	LastError  string
}

func (r Repo) Full() string { return r.Owner + "/" + r.Name }

type Triage struct {
	Summary             string   `json:"summary"`
	Category            string   `json:"category"`
	Risk                string   `json:"risk"`
	RiskReasons         []string `json:"risk_reasons"`
	NeedsMaintainer     bool     `json:"needs_maintainer"`
	MaintainerQuestions []string `json:"maintainer_questions"`
	ReviewDepth         string   `json:"review_depth"`
	FocusFiles          []string `json:"focus_files"`
	SuggestedLabels     []string `json:"suggested_labels"`
	SplitSuggestion     string   `json:"split_suggestion"`
}

type PR struct {
	ID           int64
	RepoID       int64
	Repo         string
	Number       int
	Title        string
	Body         string
	Author       string
	AuthorAssoc  string
	IsBot        bool
	URL          string
	State        string // open, merged, closed, stale
	Draft        bool
	HeadSHA      string
	BaseRef      string
	Additions    int
	Deletions    int
	ChangedFiles int
	CI           string // success, failure, pending, ""
	ReviewState  string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, ""
	Labels       []string
	CreatedAt    int64
	UpdatedAt    int64
	Col          string
	ColOverride  string
	OverrideSHA  string
	Triage       *Triage
	TriageSHA    string
	PostedSHA    string
	Requested    int // 1 my review is requested, 0 it isn't, -1 not tracked (repo scope "all")
}

type Run struct {
	ID           int64
	PRID         int64
	Kind         string // triage, review
	HeadSHA      string
	Status       string // queued, running, done, failed
	Model        string
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	Error        string
	CreatedAt    int64
	FinishedAt   int64
}

type Finding struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Inline   bool   `json:"inline"` // line exists on the right side of the diff
}

type Draft struct {
	ID        int64
	PRID      int64
	HeadSHA   string
	Verdict   string // COMMENT, APPROVE, REQUEST_CHANGES
	Body      string
	Findings  []Finding
	Notes     string
	Status    string // draft, posted, discarded
	CreatedAt int64
	PostedAt  int64
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Columns added after the first schema; "duplicate column" means already applied.
	for _, m := range []string{
		`ALTER TABLE repos ADD COLUMN scope TEXT NOT NULL DEFAULT 'requested'`,
		`ALTER TABLE prs ADD COLUMN requested INTEGER NOT NULL DEFAULT -1`,
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	// Anything left running belongs to a previous process.
	if _, err := db.Exec(`UPDATE runs SET status='queued' WHERE status='running'`); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// --- settings

func (s *Store) Config() Config {
	c := DefaultConfig()
	var raw string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='config'`).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func (s *Store) SaveConfig(c Config) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('config',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, toJSON(c))
	return err
}

// --- repos

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var policy string
	if err := sc.Scan(&r.ID, &r.Owner, &r.Name, &policy, &r.LastPolled, &r.LastError, &r.Scope); err != nil {
		return r, err
	}
	if policy != "" {
		var p Policy
		if json.Unmarshal([]byte(policy), &p) == nil {
			r.Policy = &p
		}
	}
	return r, nil
}

const repoCols = `id, owner, name, policy, last_polled, last_error, scope`

func (s *Store) Repos() ([]Repo, error) {
	rows, err := s.db.Query(`SELECT ` + repoCols + ` FROM repos ORDER BY owner, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Repo(id int64) (Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id=?`, id))
}

func (s *Store) AddRepo(owner, name, scope string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO repos(owner,name,scope) VALUES(?,?,?)`, owner, name, scope)
	return err
}

func (s *Store) SetRepoScope(id int64, scope string) error {
	_, err := s.db.Exec(`UPDATE repos SET scope=? WHERE id=?`, scope, id)
	return err
}

func (s *Store) DeleteRepo(id int64) error {
	_, err := s.db.Exec(`DELETE FROM repos WHERE id=?`, id)
	return err
}

func (s *Store) SetRepoPolicy(id int64, p *Policy) error {
	v := ""
	if p != nil {
		v = toJSON(p)
	}
	_, err := s.db.Exec(`UPDATE repos SET policy=? WHERE id=?`, v, id)
	return err
}

func (s *Store) SetRepoPolled(id int64, errMsg string) error {
	_, err := s.db.Exec(`UPDATE repos SET last_polled=?, last_error=? WHERE id=?`, now(), errMsg, id)
	return err
}

// PolicyFor resolves the effective policy for a repo.
func (s *Store) PolicyFor(repoID int64) Policy {
	if r, err := s.Repo(repoID); err == nil && r.Policy != nil {
		return *r.Policy
	}
	return s.Config().Policy
}

// --- prs

const prCols = `p.id, p.repo_id, r.owner||'/'||r.name, p.number, p.title, p.body, p.author,
	p.author_assoc, p.is_bot, p.url, p.state, p.draft, p.head_sha, p.base_ref, p.additions,
	p.deletions, p.changed_files, p.ci, p.review_state, p.labels, p.created_at, p.updated_at,
	p.col, p.col_override, p.override_sha, p.triage, p.triage_sha, p.posted_sha, p.requested
	FROM prs p JOIN repos r ON r.id = p.repo_id `

func scanPR(sc interface{ Scan(...any) error }) (PR, error) {
	var p PR
	var labels, triage string
	err := sc.Scan(&p.ID, &p.RepoID, &p.Repo, &p.Number, &p.Title, &p.Body, &p.Author,
		&p.AuthorAssoc, &p.IsBot, &p.URL, &p.State, &p.Draft, &p.HeadSHA, &p.BaseRef, &p.Additions,
		&p.Deletions, &p.ChangedFiles, &p.CI, &p.ReviewState, &labels, &p.CreatedAt, &p.UpdatedAt,
		&p.Col, &p.ColOverride, &p.OverrideSHA, &triage, &p.TriageSHA, &p.PostedSHA, &p.Requested)
	if err != nil {
		return p, err
	}
	_ = json.Unmarshal([]byte(labels), &p.Labels)
	if triage != "" {
		var t Triage
		if json.Unmarshal([]byte(triage), &t) == nil {
			p.Triage = &t
		}
	}
	return p, nil
}

func (s *Store) queryPRs(where string, args ...any) ([]PR, error) {
	rows, err := s.db.Query(`SELECT `+prCols+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PR
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PR(id int64) (PR, error) {
	return scanPR(s.db.QueryRow(`SELECT `+prCols+`WHERE p.id=?`, id))
}

// BoardPRs returns open PRs plus the ones that finished in the last week.
func (s *Store) BoardPRs() ([]PR, error) {
	return s.queryPRs(`WHERE p.state='open' OR (p.state IN ('merged','closed') AND p.updated_at > ?)
		ORDER BY p.updated_at DESC`, now()-7*86400)
}

func (s *Store) OpenPRs(repoID int64) ([]PR, error) {
	return s.queryPRs(`WHERE p.repo_id=? AND p.state='open'`, repoID)
}

// UpsertPR writes the GitHub-owned fields and leaves local state alone.
func (s *Store) UpsertPR(p PR) (int64, error) {
	var id int64
	err := s.db.QueryRow(`INSERT INTO prs(repo_id, number, title, body, author, author_assoc, is_bot,
		url, state, draft, head_sha, base_ref, additions, deletions, changed_files, ci, review_state,
		labels, created_at, updated_at, requested)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(repo_id, number) DO UPDATE SET title=excluded.title, body=excluded.body,
		author=excluded.author, author_assoc=excluded.author_assoc, is_bot=excluded.is_bot,
		url=excluded.url, state=excluded.state, draft=excluded.draft, head_sha=excluded.head_sha,
		base_ref=excluded.base_ref, additions=excluded.additions, deletions=excluded.deletions,
		changed_files=excluded.changed_files, ci=excluded.ci, review_state=excluded.review_state,
		labels=excluded.labels, created_at=excluded.created_at, updated_at=excluded.updated_at,
		requested=excluded.requested
		RETURNING id`,
		p.RepoID, p.Number, p.Title, p.Body, p.Author, p.AuthorAssoc, p.IsBot, p.URL, p.State,
		p.Draft, p.HeadSHA, p.BaseRef, p.Additions, p.Deletions, p.ChangedFiles, p.CI,
		p.ReviewState, toJSON(nonNil(p.Labels)), p.CreatedAt, p.UpdatedAt, p.Requested).Scan(&id)
	return id, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Store) SetPRState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE prs SET state=?, updated_at=? WHERE id=?`, state, now(), id)
	return err
}

func (s *Store) SetCol(id int64, col string) error {
	_, err := s.db.Exec(`UPDATE prs SET col=? WHERE id=?`, col, id)
	return err
}

func (s *Store) SetOverride(id int64, col, sha string) error {
	_, err := s.db.Exec(`UPDATE prs SET col_override=?, override_sha=? WHERE id=?`, col, sha, id)
	return err
}

func (s *Store) SetTriage(id int64, t Triage, sha string) error {
	_, err := s.db.Exec(`UPDATE prs SET triage=?, triage_sha=? WHERE id=?`, toJSON(t), sha, id)
	return err
}

func (s *Store) SetPostedSHA(id int64, sha string) error {
	_, err := s.db.Exec(`UPDATE prs SET posted_sha=? WHERE id=?`, sha, id)
	return err
}

// --- runs

// EnqueueRun queues a run unless one of the same kind is already pending for the PR.
func (s *Store) EnqueueRun(prID int64, kind, sha string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO runs(pr_id, kind, head_sha, created_at)
		SELECT ?,?,?,? WHERE NOT EXISTS
		(SELECT 1 FROM runs WHERE pr_id=? AND kind=? AND status IN ('queued','running'))`,
		prID, kind, sha, now(), prID, kind)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const runCols = `id, pr_id, kind, head_sha, status, model, cost_usd, input_tokens, output_tokens, error, created_at, finished_at`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := sc.Scan(&r.ID, &r.PRID, &r.Kind, &r.HeadSHA, &r.Status, &r.Model, &r.CostUSD,
		&r.InputTokens, &r.OutputTokens, &r.Error, &r.CreatedAt, &r.FinishedAt)
	return r, err
}

// NextRun claims the next queued run: triage before review, freshest PR first.
func (s *Store) NextRun() (Run, bool, error) {
	r, err := scanRun(s.db.QueryRow(`SELECT ` + runCols + ` FROM runs WHERE id = (
		SELECT q.id FROM runs q JOIN prs p ON p.id = q.pr_id WHERE q.status='queued'
		ORDER BY (q.kind='triage') DESC, p.updated_at DESC LIMIT 1)`))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	_, err = s.db.Exec(`UPDATE runs SET status='running' WHERE id=?`, r.ID)
	return r, err == nil, err
}

func (s *Store) FinishRun(r Run) error {
	_, err := s.db.Exec(`UPDATE runs SET status=?, head_sha=?, model=?, cost_usd=?, input_tokens=?,
		output_tokens=?, error=?, finished_at=? WHERE id=?`,
		r.Status, r.HeadSHA, r.Model, r.CostUSD, r.InputTokens, r.OutputTokens, r.Error, now(), r.ID)
	return err
}

func (s *Store) RunsForPR(prID int64) ([]Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE pr_id=? ORDER BY id DESC LIMIT 20`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasRun reports whether a run of this kind was ever queued for the commit,
// whatever its outcome. It keeps a failing run from being retried every poll.
func (s *Store) HasRun(prID int64, kind, sha string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE pr_id=? AND kind=? AND head_sha=?`, prID, kind, sha).Scan(&n)
	return n > 0
}

// DiscardPendingDrafts drops unposted drafts for a PR.
func (s *Store) DiscardPendingDrafts(prID int64) error {
	_, err := s.db.Exec(`UPDATE drafts SET status='discarded' WHERE pr_id=? AND status='draft'`, prID)
	return err
}

// ActiveRuns maps PR id to "queued" or "running".
func (s *Store) ActiveRuns() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT pr_id, status FROM runs WHERE status IN ('queued','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		if out[id] != "running" {
			out[id] = st
		}
	}
	return out, rows.Err()
}

func (s *Store) QueuedCount() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE status IN ('queued','running')`).Scan(&n)
	return n
}

func (s *Store) SpendSince(ts int64) float64 {
	var v float64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM runs WHERE finished_at >= ?`, ts).Scan(&v)
	return v
}

// --- drafts

const draftCols = `id, pr_id, head_sha, verdict, body, findings, notes, status, created_at, posted_at`

func scanDraft(sc interface{ Scan(...any) error }) (Draft, error) {
	var d Draft
	var findings string
	err := sc.Scan(&d.ID, &d.PRID, &d.HeadSHA, &d.Verdict, &d.Body, &findings, &d.Notes,
		&d.Status, &d.CreatedAt, &d.PostedAt)
	if err == nil {
		_ = json.Unmarshal([]byte(findings), &d.Findings)
	}
	return d, err
}

// CreateDraft replaces any unposted draft for the PR.
func (s *Store) CreateDraft(d Draft) (int64, error) {
	if _, err := s.db.Exec(`UPDATE drafts SET status='discarded' WHERE pr_id=? AND status='draft'`, d.PRID); err != nil {
		return 0, err
	}
	if d.Findings == nil {
		d.Findings = []Finding{}
	}
	res, err := s.db.Exec(`INSERT INTO drafts(pr_id, head_sha, verdict, body, findings, notes, created_at)
		VALUES(?,?,?,?,?,?,?)`, d.PRID, d.HeadSHA, d.Verdict, d.Body, toJSON(d.Findings), d.Notes, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Draft(id int64) (Draft, error) {
	return scanDraft(s.db.QueryRow(`SELECT `+draftCols+` FROM drafts WHERE id=?`, id))
}

// LatestDraft returns the newest non-discarded draft for a PR.
func (s *Store) LatestDraft(prID int64) (Draft, bool) {
	d, err := scanDraft(s.db.QueryRow(`SELECT `+draftCols+` FROM drafts
		WHERE pr_id=? AND status != 'discarded' ORDER BY id DESC LIMIT 1`, prID))
	return d, err == nil
}

func (s *Store) SetDraftStatus(id int64, status string) error {
	posted := int64(0)
	if status == "posted" {
		posted = now()
	}
	_, err := s.db.Exec(`UPDATE drafts SET status=?, posted_at=? WHERE id=?`, status, posted, id)
	return err
}

// PendingDrafts maps PR id to the head SHA its unposted draft was written against.
func (s *Store) PendingDrafts() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT pr_id, head_sha FROM drafts WHERE status='draft'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var sha string
		if err := rows.Scan(&id, &sha); err != nil {
			return nil, err
		}
		out[id] = sha
	}
	return out, rows.Err()
}

// SplitRepo parses "owner/name" or a github.com URL.
func SplitRepo(s string) (owner, name string, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

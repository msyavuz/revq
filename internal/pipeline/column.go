package pipeline

import "github.com/msyavuz/revq/internal/store"

const (
	ColInbox  = "inbox"
	ColYou    = "you"
	ColAuthor = "author"
	ColMerge  = "merge"
	ColDone   = "done"
)

var Columns = []struct{ Key, Title, Hint, Empty string }{
	{ColInbox, "Inbox", "Waiting for the agent", "PRs sit here while the agent takes its first look."},
	{ColYou, "Needs you", "Your decision or review", "Nothing is waiting on you."},
	{ColAuthor, "Waiting on author", "Reviewed, draft, red CI, or changes requested", "PRs you've reviewed wait here until the author asks again."},
	{ColMerge, "Ready to merge", "Approved and green", "Approved PRs with green CI land here."},
	{ColDone, "Done", "Merged or closed this week", "Merged and closed PRs from the last 7 days."},
}

func ValidColumn(c string) bool {
	for _, col := range Columns {
		if col.Key == c {
			return true
		}
	}
	return false
}

// Column decides where a PR sits. A manual move wins until the author pushes.
func Column(pr store.PR, pendingDraft, activeRun bool) string {
	switch {
	case pr.State != "open":
		return ColDone
	case pr.ColOverride != "" && pr.OverrideSHA == pr.HeadSHA:
		return pr.ColOverride
	case pendingDraft:
		return ColYou
	case activeRun && pr.TriageSHA == "":
		return ColInbox
	case pr.Requested == 1 && !pr.Draft:
		// An explicit request beats approvals by others, red CI, or requested changes.
		return ColYou
	case pr.ReviewState == "APPROVED" && pr.CI != "failure" && !pr.Draft:
		return ColMerge
	case pr.Requested == 0:
		// I've reviewed it and nobody has asked me again.
		return ColAuthor
	case pr.Draft, pr.CI == "failure", pr.ReviewState == "CHANGES_REQUESTED":
		return ColAuthor
	case pr.PostedSHA != "" && pr.PostedSHA == pr.HeadSHA:
		// We reviewed this exact commit; nothing new to look at.
		return ColAuthor
	}
	return ColYou
}

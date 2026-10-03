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
	{ColInbox, "Inbox", "Review requested, nothing drafted yet", "New review requests land here. Press Draft review on a card, or turn on auto review in Settings."},
	{ColYou, "Needs you", "A drafted review or a decision is waiting for you", "Nothing is waiting on you. Drafted reviews show up here."},
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

// Column decides where a PR sits. The board reads left to right: a review
// request arrives in the inbox, moves to "needs you" once there is something
// for the maintainer to act on (a drafted review or a flagged decision), then
// to the author, then to merge. A manual move wins until the author pushes.
func Column(pr store.PR, pendingDraft bool) string {
	mergeable := pr.ReviewState == "APPROVED" && pr.CI != "failure" && !pr.Draft
	switch {
	case pr.State != "open":
		return ColDone
	case pr.ColOverride != "" && pr.OverrideSHA == pr.HeadSHA:
		return pr.ColOverride
	case pendingDraft:
		return ColYou
	case pr.PostedSHA != "" && pr.PostedSHA == pr.HeadSHA:
		// We reviewed this exact commit; nothing new to look at.
		if mergeable {
			return ColMerge
		}
		return ColAuthor
	case pr.Assessment != nil && pr.AssessedSHA == pr.HeadSHA && pr.Assessment.NeedsMaintainer && pr.Requested != 0:
		return ColYou
	case pr.Requested == 1 && !pr.Draft:
		// An explicit request beats approvals by others, red CI, or requested changes.
		return ColInbox
	case mergeable:
		return ColMerge
	case pr.Requested == 0, pr.Draft, pr.CI == "failure", pr.ReviewState == "CHANGES_REQUESTED":
		// Reviewed and not asked again, or not reviewable yet.
		return ColAuthor
	}
	return ColInbox
}

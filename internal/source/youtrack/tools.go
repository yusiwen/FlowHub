package youtrack

// AllowedMCPTools are the YouTrack tools the agent may call without asking.
//
// Everything absent from this list stays gated: the session ruleset asks and the
// arbiter decides. The measured inventory includes tracker writers
// (`youtrack_update_issue`), web crawlers and cross-session memory, none of which
// an unattended turn needs.
var AllowedMCPTools = []string{
	"youtrack_get_issue",
	"youtrack_get_issue_comments",
	"youtrack_search_issues",
	"youtrack_get_project",
	"youtrack_get_issue_fields_schema",
	"youtrack_find_projects",
	"youtrack_get_current_user",
	"youtrack_add_issue_comment",
}

// ReplyTools are the tool calls that mean "this turn replied". A completed call to
// any of them is what the dispatcher reports as the turn's answer, which is the
// signal that the issue actually got a comment.
var ReplyTools = []string{"youtrack_add_issue_comment"}

// DownloadHosts is the YouTrack host whose signed attachment URLs the agent is
// allowed to download from. An empty list disables downloads entirely.
var DownloadHosts = []string{"pm.yusiwen.cn"}

// DownloadPrefix is the only path inside the worktree a download may be written to.
const DownloadPrefix = ".flowhub/attachments"

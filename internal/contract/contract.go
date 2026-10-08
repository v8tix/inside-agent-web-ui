// Package contract is the UI test contract between agent-web-ui and agent-eval:
// the hooks the evaluator relies on to drive and read the page.
//
// The evaluator clicks and types by accessible name (it works from the
// accessibility tree, where elements are found by uid) and reads state through
// data-testid and data attributes. A test in the Web UI suite renders every
// page state and fails when a hook here is missing, misnamed or lacks its
// attributes, so a template change cannot silently break the evaluator.
//
// # Changing the contract
//
//   - Adding a hook, or a state in which an existing hook appears, is
//     compatible: the version stays.
//   - Removing or renaming a hook, changing an accessible name, a data
//     attribute or its meaning, is a breaking change: raise Version, update the
//     evaluator in the same change, and say so in the commit message. The
//     evaluator refuses a page whose data-ui-contract differs from the version
//     it was built for.
package contract

// Version is the value of <body data-ui-contract="...">.
const Version = "1"

// Page states a hook can be required in. The contract test renders each.
const (
	// Login is the sign-in page.
	Login = "login"
	// Empty is the start page: no conversation open, one in the sidebar.
	Empty = "empty"
	// Finished is a conversation whose last reply is complete.
	Finished = "finished"
	// Failed is a conversation whose last reply failed.
	Failed = "failed"
	// Statuses is a conversation showing every message status at once.
	Statuses = "statuses"
	// RenameForm is the fragment that turns a sidebar row into a title field.
	RenameForm = "rename-form"
	// DeleteDialog is the fragment with the delete confirmation.
	DeleteDialog = "delete-dialog"
)

// Hook is one thing the evaluator depends on.
type Hook struct {
	// TestID is the data-testid value.
	TestID string
	// States lists the page states in which the hook must be present.
	States []string
	// Tag is the element's name (button, textarea, a, ...). Empty: any.
	Tag string
	// Name is the accessible name. {title} stands for the conversation's title.
	// Empty: no name is required (containers and read-only elements).
	Name string
	// NamePrefix says Name is only how the accessible name starts.
	NamePrefix bool
	// Attrs are attributes the element must carry.
	Attrs []string
}

// Hooks is the contract, version 1.
//
// Pattern: UI Contract Table
var Hooks = []Hook{
	// Signing in.
	{TestID: "login-email", States: []string{Login}, Tag: "input", Name: "Email"},
	{TestID: "login-password", States: []string{Login}, Tag: "input", Name: "Password"},
	{TestID: "login-submit", States: []string{Login}, Tag: "button", Name: "Sign in"},

	// Every page.
	{TestID: "theme-toggle", States: []string{Login, Empty, Finished}, Tag: "button", Name: "Theme", NamePrefix: true},
	{TestID: "live-status", States: []string{Login, Empty, Finished}, Tag: "div", Attrs: []string{"role", "aria-live"}},
	{TestID: "logout", States: []string{Empty, Finished}, Tag: "button", Name: "Sign out"},

	// The composer.
	{TestID: "composer-input", States: []string{Empty, Finished}, Tag: "textarea", Name: "Message"},
	{TestID: "send", States: []string{Empty, Finished}, Tag: "button", Name: "Send message"},
	{TestID: "stop", States: []string{Empty, Finished}, Tag: "button", Name: "Stop generating"},
	{TestID: "composer-error", States: []string{Empty, Finished}, Attrs: []string{"role"}},

	// The conversation.
	{TestID: "messages", States: []string{Finished, Failed, Statuses}},
	// Hidden until the reader scrolls up, so it has no accessible name to check yet.
	{TestID: "jump-latest", States: []string{Finished, Failed, Statuses}, Tag: "button"},

	// Messages.
	{TestID: "message", States: []string{Finished, Failed, Statuses}, Tag: "article", Attrs: []string{"data-role", "data-id", "data-status"}},
	{TestID: "message-body", States: []string{Finished, Failed, Statuses}},
	{TestID: "error", States: []string{Failed, Statuses}, Attrs: []string{"role"}},
	{TestID: "regenerate", States: []string{Finished}, Tag: "button", Name: "Regenerate response"},
	{TestID: "retry", States: []string{Failed}, Tag: "button", Name: "Retry"},
	{TestID: "copy-message", States: []string{Finished, Failed, Statuses}, Tag: "button", Name: "Copy response", Attrs: []string{"data-raw-url"}},

	// The sidebar.
	{TestID: "sidebar", States: []string{Empty, Finished}, Tag: "nav"},
	{TestID: "sidebar-toggle", States: []string{Empty, Finished}, Tag: "button", Name: "Open conversations"},
	{TestID: "new-chat", States: []string{Empty, Finished}, Tag: "a", Name: "New chat"},
	{TestID: "conversation", States: []string{Empty, Finished}, Tag: "li", Attrs: []string{"data-id"}},
	{TestID: "conversation-link", States: []string{Empty, Finished}, Tag: "a", Name: "{title}"},
	{TestID: "rename", States: []string{Empty, Finished}, Tag: "button", Name: "Rename conversation {title}"},
	{TestID: "delete", States: []string{Empty, Finished}, Tag: "button", Name: "Delete conversation {title}"},

	// Renaming and deleting.
	{TestID: "rename-input", States: []string{RenameForm}, Tag: "input", Name: "Conversation title"},
	{TestID: "rename-save", States: []string{RenameForm}, Tag: "button", Name: "Save"},
	{TestID: "rename-cancel", States: []string{RenameForm}, Tag: "button", Name: "Cancel"},
	{TestID: "rename-error", States: []string{RenameForm}, Attrs: []string{"role"}},
	{TestID: "delete-dialog", States: []string{DeleteDialog}, Attrs: []string{"aria-labelledby"}},
	{TestID: "delete-confirm", States: []string{DeleteDialog}, Tag: "button", Name: "Delete"},
	{TestID: "delete-cancel", States: []string{DeleteDialog}, Tag: "button", Name: "Cancel"},
}

// Statuses a message can have, as they appear in data-status.
var MessageStatuses = []string{"pending", "streaming", "complete", "stopped", "failed"}

// Telemetry holds the data attributes an assistant message carries once it is
// finished, when the value is known. They are absent, never zero or empty,
// when it is not.
var Telemetry = []string{
	"data-ttft-ms", "data-duration-ms", "data-prompt-tokens", "data-completion-tokens", "data-tool-calls",
}

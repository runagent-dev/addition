package cli

// Exit codes are part of the agent-facing contract (docs/cli-plan.md §2.5).
const (
	ExitOK             = 0
	ExitInternal       = 1
	ExitUsage          = 2
	ExitNeedsConnect   = 3
	ExitUpstreamClient = 4
	ExitUpstreamServer = 5
	ExitPolicyDenied   = 6
)

// Error is a failure with a stable machine-readable code and exit status.
type Error struct {
	Code    string
	Message string
	Exit    int
}

func (e *Error) Error() string { return e.Message }

func usageError(message string) *Error {
	return &Error{Code: "usage", Message: message, Exit: ExitUsage}
}

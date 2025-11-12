package agent

import (
	"context"
	"os/exec"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/tui/page"
)

// State is the state of the agent.
type State string

// InitMsg is the first message sent to the agent to initialize it.
type InitMsg struct{}

// ReadyMsg is sent by the agent when it is ready to receive user input.
type ReadyMsg struct {
	Models  []string
	Version string
}

// ConfigUpdatedMsg is sent when the config is updated.
type ConfigUpdatedMsg struct {
	Config config.Config
}

// StateChangeMsg is sent when the agent's state changes.
type StateChangeMsg State

// ErrorMsg is sent when an error occurs in the agent.
type ErrorMsg struct{ error }

// InfoMsg is sent when the agent has information to display to the user.
type InfoMsg string

// RunningCommandMsg is sent when a command is being run.
type RunningCommandMsg struct {
	Cmd *exec.Cmd
}

// ToolRequestMsg is sent when a tool is being requested.
type ToolRequestMsg struct {
	Call *fantasy.ToolCall
}

// ToolResponseMsg is sent when a tool has responded.
type ToolResponseMsg struct {
	Call   *fantasy.ToolCall
	Result string
}

// HistoryLoaded is sent when the history is loaded from the database.
type HistoryLoaded struct {
	History []message.Message
}

// PageLoaded is sent when a page is loaded.
type PageLoaded struct {
	Page page.PageID
}

// UserInputStarted is sent when the agent starts processing user input.
type UserInputStarted struct {
	Cancel context.CancelFunc
}

// UserInputFinished is sent when the agent finishes processing user input.
type UserInputFinished struct{}

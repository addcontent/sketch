package tui

// Package tui implements a Bubble Tea based terminal user interface for Sketch.
//
// This is an initial scaffolding that mirrors the public interface of termui.
// It presents a chat viewport on the top and a one-line text input at the
// bottom.  Agent responses and log lines are streamed into the viewport via a
// channel from a background goroutine.
//
// The implementation purposefully starts small: only plain chat lines, no
// colours, no shell-escape commands and no budget/cost footer yet.  Those
// features can be ported iteratively once the basics are rock-solid.

import (
    "context"
    "fmt"
    "strings"
    "time"

    tea "github.com/charmbracelet/bubbletea"
    "github.com/charmbracelet/bubbles/textinput"

    "sketch.dev/loop"
)

// chatMessage is copied from termui for compatibility.
type chatMessage struct {
    idx      int
    sender   string
    content  string
    thinking bool
}

// BubbleUI is the public type returned by New.  Keep the same surface area as
// termui.TermUI so callers can switch without big refactors.
//
// The zero value is not usable; always construct via New().
type BubbleUI struct {
    agent   loop.CodingAgent
    httpURL string
}

// New constructs a BubbleUI bound to the given CodingAgent.
func New(agent loop.CodingAgent, httpURL string) *BubbleUI {
    return &BubbleUI{agent: agent, httpURL: httpURL}
}

// Run starts the Bubble Tea program and blocks until it exits.
// It matches the signature of (*termui.TermUI).Run.
// RestoreOldState is present only so BubbleUI matches the minimal
// interface expected by main.go. It does nothing because Bubble Tea
// restores the terminal state automatically.
func (ui *BubbleUI) RestoreOldState() error { return nil }

func (ui *BubbleUI) Run(ctx context.Context) error {
    m := initialModel(ui.agent, ui.httpURL)

    // Bubble Tea uses its own context cancellation via tea.Quit().  We still
    // honour the supplied ctx by stopping the program when ctx is done.
    p := tea.NewProgram(m, tea.WithAltScreen())

    // Forward ctx cancellation.
    go func() {
        <-ctx.Done()
        _ = p.Quit()
    }()

    if _, err := p.Run(); err != nil {
        return fmt.Errorf("bubble ui: %w", err)
    }
    return nil
}

//-----------------------------------------------------------------------------
// Bubble Tea model implementation
//----------------------------------------------------------------------------- 

type model struct {
    agent   loop.CodingAgent
    httpURL string

    // viewport state
    chat []string
    logs []string

    // user input component
    input textinput.Model

    thinking bool
    width    int
    height   int

    // channel that receives messages from agent listener goroutine
    incoming chan tea.Msg
}

// Compile-time check that *model implements tea.Model.
var _ tea.Model = (*model)(nil)

func initialModel(agent loop.CodingAgent, httpURL string) model {
    ti := textinput.New()
    ti.Prompt = "> "
    ti.Focus()

    m := model{
        agent:   agent,
        httpURL: httpURL,
        chat:    []string{fmt.Sprintf("🌐 %s/", httpURL), "💬 type 'help' for help", ""},
        input:   ti,
        incoming: make(chan tea.Msg, 32),
    }

    // Start goroutine that pumps agent events to the model via incoming channel.
    go agentListener(agent, m.incoming)

    return m
}

func (m model) Init() tea.Cmd {
    // Combine textinput's blinking cursor cmd with waiting for incoming msgs.
    return tea.Batch(textinput.Blink, m.waitForMsg())
}

// waitForMsg returns a Cmd that waits for the next message from the incoming
// channel.
func (m model) waitForMsg() tea.Cmd {
    return func() tea.Msg {
        return <-m.incoming
    }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.KeyMsg:
        switch msg.Type {
        case tea.KeyCtrlC:
            return m, tea.Quit
        case tea.KeyEnter:
            line := strings.TrimSpace(m.input.Value())
            m.input.Reset()
            if line == "" {
                return m, nil
            }
            // Echo user input into chat viewport.
            m.chat = append(m.chat, "🦸 "+line)
            // Forward to LLM agent.
            m.agent.UserMessage(context.Background(), line)
            return m, m.waitForMsg()
        }

    case chatMessage:
        // Agent conversational response.
        prefix := "🕴️ "
        m.chat = append(m.chat, prefix+msg.content)
        m.thinking = msg.thinking
        return m, m.waitForMsg()

    case logMsg:
        m.logs = append(m.logs, string(msg))
        // Also show logs inline for now.
        m.chat = append(m.chat, string(msg))
        return m, m.waitForMsg()

    case tea.WindowSizeMsg:
        m.width, m.height = msg.Width, msg.Height
        return m, nil
    }

    // Let textinput consume any key events.
    var cmd tea.Cmd
    m.input, cmd = m.input.Update(msg)
    return m, cmd
}

func (m model) View() string {
    var b strings.Builder
    // Display chat lines, clipping to terminal height-3 to leave space for input
    maxLines := m.height - 2
    if maxLines <= 0 {
        maxLines = len(m.chat)
    }
    start := 0
    if len(m.chat) > maxLines {
        start = len(m.chat) - maxLines
    }
    for _, line := range m.chat[start:] {
        b.WriteString(line)
        b.WriteByte('\n')
    }
    b.WriteString("\n")
    b.WriteString(m.input.View())
    if m.thinking {
        b.WriteString(" *")
    }
    return b.String()
}

//-----------------------------------------------------------------------------
// Agent listener
//----------------------------------------------------------------------------- 

type logMsg string

type errMsg struct{ error }

func agentListener(agent loop.CodingAgent, ch chan<- tea.Msg) {
    ctx := context.Background()
    it := agent.NewIterator(ctx, 0)
    ticker := time.NewTicker(500 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            // periodically wake up UI in case it needs to redraw while thinking
            ch <- tea.TickMsg{Time: time.Now()}
        default:
        }

        resp := it.Next()
        if resp == nil {
            close(ch)
            return
        }
        if resp.HideOutput {
            continue
        }

        thinking := !(resp.EndOfTurn && resp.ParentConversationID == nil)

        switch resp.Type {
        case loop.AgentMessageType:
            ch <- chatMessage{idx: resp.Idx, sender: "🕴️", content: resp.Content, thinking: thinking}
        case loop.ToolUseMessageType, loop.ErrorMessageType, loop.BudgetMessageType,
            loop.AutoMessageType, loop.CommitMessageType:
            // For now funnel everything to logMsg.
            ch <- logMsg(resp.Content)
        default:
            ch <- logMsg(fmt.Sprintf("❌ unexpected message type %s", resp.Type))
        }
    }
}

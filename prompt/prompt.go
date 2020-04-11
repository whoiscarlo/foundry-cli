package prompt

import (
	"fmt"
	"foundry/cli/logger"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"foundry/cli/prompt/cmd"

	goprompt "github.com/mlejva/go-prompt"
)

type CursorPos struct {
	Row int
	Col int
}

func CursorOutputStart() CursorPos {
	return CursorPos{1, 1}
}

type PromptEventType string

type PromptEvent struct {
	Type PromptEventType
}
type Prompt struct {
	cmds []cmd.Cmd

	outBuf *Buffer
	// outBufMutex sync.Mutex

	renderMutex sync.Mutex

	promptPrefix string
	promptText   string
	promptRow    int // Will be recalculated once the terminal is ready

	infoText string
	infoRow  int // Will be recalculated once the terminal is ready

	totalColumns int // Will be recalculated once the terminal is ready
	totalRows    int // Will be recalculated once the terminal is ready
	freeRows     int // Will be recalculated once the terminal is ready

	parser *goprompt.PosixParser
	writer goprompt.ConsoleWriter

	savedPos   CursorPos
	currentPos CursorPos // Current position of the cursor when printing output

	lastEscapeCode string // Last VT100 terminal escape code that should be applied next time the print() method is called

	Events chan PromptEvent
}

const (
	PromptEventTypeRerender PromptEventType = "rerender"
)

//////////////////////

func (p *Prompt) completer(d goprompt.Document) []goprompt.Suggest {
	p.renderMutex.Lock()
	p.promptText = d.CurrentLine()
	p.renderMutex.Unlock()

	return []goprompt.Suggest{}
}

func (p *Prompt) executor(s string) {
	if s == "" {
		return
	}
	logger.Fdebugln("Executor:", s)

	fields := strings.Fields(s)

	if cmd := p.getCommand(fields[0]); cmd != nil {
		logger.Fdebugln("cmd:", cmd)
		args := fields[1:]
		logger.Fdebugln("args:", args)
		cmd.RunRequest(args)
	} else {
		// Delete an old info message and show the new one

		p.renderMutex.Lock()

		// Delete an old info message
		p.writer.CursorGoTo(p.infoRow, 1)
		p.writer.EraseLine()

		// Print the new info message
		p.writer.SetColor(goprompt.Red, goprompt.DefaultColor, true)
		p.infoText = fmt.Sprintf("Unknown command '%s'", fields[0])
		p.writer.WriteRawStr(p.infoText)
		p.writer.SetColor(goprompt.DefaultColor, goprompt.DefaultColor, false)

		// Move cursor back to the prompt
		p.writer.CursorGoTo(p.promptRow, len(p.promptPrefix)+len(p.promptText)+1)

		if err := p.writer.Flush(); err != nil {
			logger.FdebuglnFatal("Error flushing prompt buffer", err)
			logger.FatalLogln("Error flushing prompt buffer", err)
		}

		p.renderMutex.Unlock()
	}
}

func (p *Prompt) getCommand(s string) cmd.Cmd {
	for _, c := range p.cmds {
		if c.Name() == s {
			return c
		}
	}
	return nil
}

/////////////

func NewPrompt(cmds []cmd.Cmd) *Prompt {
	prefix := "> "
	return &Prompt{
		cmds: cmds,

		outBuf: NewBuffer(),

		promptPrefix: prefix,

		parser: goprompt.NewStandardInputParser(),
		writer: goprompt.NewStandardOutputWriter(),

		// Terminal is indexed from 1
		savedPos:   CursorOutputStart(),
		currentPos: CursorPos{1, len(prefix) + 1},

		Events: make(chan PromptEvent),
	}
}

func (p *Prompt) Run() {
	// Read buffer and print anything that gets send to the channel
	bufCh := make(chan []byte, 128)
	stopReadCh := make(chan struct{})
	go p.outBuf.Read(bufCh, stopReadCh)
	go func() {
		for {
			select {
			case b := <-bufCh:
				p.print(b)
			default:
				time.Sleep(time.Millisecond * 10)
			}
		}
	}()

	interupOpt := goprompt.OptionAddKeyBind(goprompt.KeyBind{
		Key: goprompt.ControlC,
		Fn: func(buf *goprompt.Buffer) {
			os.Exit(0)
		},
	})
	prefixOpt := goprompt.OptionPrefix(p.promptPrefix)
	prefixColOpt := goprompt.OptionPrefixTextColor(goprompt.Green)
	prompt := goprompt.New(p.executor, p.completer, interupOpt, prefixOpt, prefixColOpt)
	go prompt.Run()

	// The initial rerender for the current terminal size
	if err := p.rerender(true); err != nil {
		logger.Fdebugln("Error during the initial rerender", err)
		logger.FatalLogln("Error during the initial rerender", err)
	}

	// Rerender a terminal for every size change
	go p.rerenderOnTermSizeChange()
}

func (p *Prompt) Writeln(s string) (n int, err error) {
	return p.outBuf.Write([]byte(s))
}

func (p *Prompt) SetInfoln(s string) error {
	p.renderMutex.Lock()
	defer p.renderMutex.Unlock()

	p.writer.CursorGoTo(p.infoRow, 1)
	p.writer.EraseLine()

	p.writer.SetColor(goprompt.Green, goprompt.DefaultColor, true)
	t := strings.TrimSpace(s)
	p.writer.WriteStr(t)
	p.writer.SetColor(goprompt.DefaultColor, goprompt.DefaultColor, true)

	p.writer.CursorGoTo(p.promptRow, len(p.promptPrefix)+len(p.promptText)+1)

	return p.writer.Flush()
}

func (p *Prompt) rerender(initialRun bool) error {
	p.renderMutex.Lock()
	defer p.renderMutex.Unlock()

	size := p.parser.GetWinSize()
	if initialRun {
		p.moveWindowDown(int(size.Row))
	}

	p.writer.EraseScreen()

	p.currentPos = CursorOutputStart()
	p.savedPos = CursorOutputStart()

	p.totalRows = int(size.Row)
	p.totalColumns = int(size.Col)
	p.promptRow = p.totalRows
	p.infoRow = p.totalRows - 1
	p.freeRows = p.totalRows

	// Move to the info row and restore the text
	p.writer.CursorGoTo(p.infoRow, 1)
	p.writer.SetColor(goprompt.Red, goprompt.DefaultColor, true)
	p.writer.WriteRawStr(p.infoText)

	p.writer.CursorGoTo(p.promptRow, 1)

	if err := p.writer.Flush(); err != nil {
		return err
	}

	p.Events <- PromptEvent{PromptEventTypeRerender}
	return nil
}

// Prints # of rows of "\n" - this way the visible terminal window
// is moved down and the previous user's terminal history isn't
//  erased on the initial rerender()
func (p *Prompt) moveWindowDown(rows int) error {
	p.writer.CursorGoTo(rows, 0)
	p.writer.WriteRawStr(strings.Repeat("\n", rows))
	return p.writer.Flush()
}

func (p *Prompt) rerenderOnTermSizeChange() {
	sigwinchCh := make(chan os.Signal, 1)
	defer close(sigwinchCh)
	signal.Notify(sigwinchCh, syscall.SIGWINCH)
	for {
		if _, ok := <-sigwinchCh; !ok {
			return
		}
		if err := p.rerender(false); err != nil {
			logger.FdebuglnFatal("Error during the rerender", err)
			logger.FatalLogln("Error during the rerender", err)
		}
	}
}

func (p *Prompt) print(b []byte) {
	p.renderMutex.Lock()
	defer p.renderMutex.Unlock()

	// The invariant is that the the p.savedPos always holds
	// a position where we stopped printing the text = where
	// we should start printing text again.
	p.writer.CursorGoTo(p.savedPos.Row, p.savedPos.Col)

	s := string(b)
	// s = "\n====================\nLorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur. Excepteur \nsint occaecat cupidatat non proident, sunt in culpa qui officia deserunt mollit anim id est laborum."
	logger.Fdebugln(s)

	escapeStart := false
	for _, r := range s {
		p.writer.WriteRawStr(p.lastEscapeCode)
		p.writer.WriteRawStr(string(r))

		// Don't increase p.currentPos.Col while we are processing a terminal VT100 escape code
		if r == '\u001b' {
			// Reset the the last escape code
			p.lastEscapeCode = string('\u001b')
			escapeStart = true
			continue
		}

		if escapeStart {
			p.lastEscapeCode += string(r)
			// 'm' character signals that the escaped code is ending
			if r == 'm' {
				escapeStart = false
				continue
			} else {
				continue
			}
		}

		p.currentPos.Col++

		if r == '\n' {
			// On a new line, the cursor moves to the start of a line
			p.currentPos.Col = 1

			p.currentPos.Row++
			p.freeRows--
		}

		// TODO: Is this required?
		// This hardcoded solution makes it impossible to have resizable text
		// as you resize your terminal
		if p.currentPos.Col == p.totalColumns {
			// Make a new line
			p.writer.WriteRawStr("\n")
			p.currentPos.Col = 1
			p.currentPos.Row++
			p.freeRows--
		}

		if p.freeRows == 2 {
			p.savedPos = p.currentPos
			// Go to a prompt row and create a new line so that we
			// once again have 3 free rows.
			// The reason we have to go to the prompt row is becauase
			// if we had printed a new line anywhere before the prompt
			// row, the cursor would simply move down without actually
			// creating a new line in the terminal.

			// Erase the info row and prompt row so that a text doesn't stay there
			// when the everything is moved up by 1 row
			p.writer.CursorGoTo(p.infoRow, 1)
			p.writer.EraseLine()
			p.writer.CursorGoTo(p.promptRow, 1)
			p.writer.EraseLine()

			// Create a new line
			p.writer.WriteRawStr("\n")

			// Move cursor back to a position where we stopped outputting
			// text. This will be next available new line after the last
			// line of printed text
			p.writer.CursorGoTo(p.savedPos.Row, p.savedPos.Col)
			// The reason it's not sufficient to just go to p.savedPos
			// is because we printed a newline. All text moved 1 line up.
			p.writer.CursorUp(1)

			p.currentPos.Row--
			p.currentPos.Col = 1
			p.freeRows = 3
		}
	}
	p.savedPos = p.currentPos

	// Move to the info row and restore the info text
	p.writer.CursorGoTo(p.infoRow, 1)
	p.writer.SetColor(goprompt.Red, goprompt.DefaultColor, true)
	p.writer.WriteRawStr(p.infoText)

	// Move to the prompt row and restore the text
	p.writer.CursorGoTo(p.promptRow, 1)
	p.writer.SetColor(goprompt.Green, goprompt.DefaultColor, false)
	p.writer.WriteRawStr(p.promptPrefix)
	p.writer.SetColor(goprompt.DefaultColor, goprompt.DefaultColor, false)
	p.writer.WriteRawStr(p.promptText)

	if err := p.writer.Flush(); err != nil {
		logger.FdebuglnFatal("Error flushing prompt buffer (2)", err)
		logger.FatalLogln("Error flushing prompt buffer", err)
	}
}

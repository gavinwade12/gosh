package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"plugin"
	"regexp"
	"strings"
	"syscall"

	"github.com/vladimirvivien/gosh/api"
)

var (
	reCmd = regexp.MustCompile(`\S+`)
)

type Goshell struct {
	ctx        context.Context
	pluginsDir string
	commands   map[string]api.Command
	closed     chan struct{}
}

// New returns a new shell
func New() *Goshell {
	return &Goshell{
		pluginsDir: api.PluginsDir,
		commands:   make(map[string]api.Command),
		closed:     make(chan struct{}),
	}
}

// Init initializes the shell with the given context
func (gosh *Goshell) Init(ctx context.Context) error {
	gosh.ctx = ctx
	gosh.printSplash()
	return gosh.loadCommands()
}

func (gosh *Goshell) loadCommands() error {
	if _, err := os.Stat(gosh.pluginsDir); err != nil {
		return err
	}

	plugins, err := listFiles(gosh.pluginsDir, `.*_command.so`)
	if err != nil {
		return err
	}

	for _, cmdPlugin := range plugins {
		plug, err := plugin.Open(path.Join(gosh.pluginsDir, cmdPlugin.Name()))
		if err != nil {
			fmt.Printf("failed to open plugin %s: %v\n", cmdPlugin.Name(), err)
			continue
		}
		cmdSymbol, err := plug.Lookup(api.CmdSymbolName)
		if err != nil {
			fmt.Printf("plugin %s does not export symbol \"%s\"\n",
				cmdPlugin.Name(), api.CmdSymbolName)
			continue
		}
		commands, ok := cmdSymbol.(api.Commands)
		if !ok {
			fmt.Printf("Symbol %s (from %s) does not implement Commands interface\n",
				api.CmdSymbolName, cmdPlugin.Name())
			continue
		}
		if err := commands.Init(gosh.ctx); err != nil {
			fmt.Printf("%s initialization failed: %v\n", cmdPlugin.Name(), err)
			continue
		}
		for name, cmd := range commands.Registry() {
			gosh.commands[name] = cmd
		}
		gosh.ctx = context.WithValue(gosh.ctx, "gosh.commands", gosh.commands)
	}
	return nil
}

// TODO delegate splash to a plugin
func (gosh *Goshell) printSplash() {
	fmt.Println(`	
                        888      
                        888      
                        888      
 .d88b.  .d88b. .d8888b 88888b.  
d88P"88bd88""88b88K     888 "88b 
888  888888  888"Y8888b.888  888 
Y88b 888Y88..88P     X88888  888 
 "Y88888 "Y88P"  88888P'888  888 
     888                         
Y8b d88P                         
 "Y88P"
 
 `)
}

// Open opens the shell for the given reader
func (gosh *Goshell) Open(r *bufio.Reader) {
	loopCtx := gosh.ctx
	line := make(chan string)
	for {
		// start a goroutine to get input from the user
		go func(ctx context.Context, input chan<- string) {
			for {
				// TODO: future enhancement is to capture input key by key
				// to give command granular notification of key events.
				// This could be used to implement command autocompletion.
				fmt.Fprintf(ctx.Value("gosh.stdout").(io.Writer), "%s ", api.GetPrompt(loopCtx))
				line, err := r.ReadString('\n')
				if err != nil {
					fmt.Fprintf(ctx.Value("gosh.stderr").(io.Writer), "%v\n", err)
					continue
				}

				input <- line
				return
			}
		}(loopCtx, line)

		// wait for input or cancel
		select {
		case <-gosh.ctx.Done():
			close(gosh.closed)
			return
		case input := <-line:
			var err error
			loopCtx, err = gosh.handle(loopCtx, input)
			if err != nil {
				fmt.Fprintf(loopCtx.Value("gosh.stderr").(io.Writer), "%v\n", err)
			}
		}
	}
}

// Closed returns a channel that closes when the shell has closed
func (gosh *Goshell) Closed() <-chan struct{} {
	return gosh.closed
}

func (gosh *Goshell) handle(ctx context.Context, cmdLine string) (context.Context, error) {
	line := strings.TrimSpace(cmdLine)
	if line == "" {
		return ctx, nil
	}
	args := reCmd.FindAllString(line, -1)
	if args != nil {
		cmdName := args[0]
		cmd, ok := gosh.commands[cmdName]
		if !ok {
			return ctx, errors.New(fmt.Sprintf("command not found: %s", cmdName))
		}
		return cmd.Exec(ctx, args)
	}
	return ctx, errors.New(fmt.Sprintf("unable to parse command line: %s", line))
}

func listFiles(dir, pattern string) ([]os.FileInfo, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	filteredFiles := []os.FileInfo{}
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		matched, err := regexp.MatchString(pattern, file.Name())
		if err != nil {
			return nil, err
		}
		if matched {
			filteredFiles = append(filteredFiles, file)
		}
	}
	return filteredFiles, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = context.WithValue(ctx, "gosh.prompt", api.DefaultPrompt)
	ctx = context.WithValue(ctx, "gosh.stdout", os.Stdout)
	ctx = context.WithValue(ctx, "gosh.stderr", os.Stderr)
	ctx = context.WithValue(ctx, "gosh.stdin", os.Stdin)

	shell := New()
	if err := shell.Init(ctx); err != nil {
		fmt.Print("\n\nfailed to initialize:", err)
		os.Exit(1)
	}

	// prompt for help
	cmdCount := len(shell.commands)
	if cmdCount > 0 {
		if _, ok := shell.commands["help"]; ok {
			fmt.Printf("\nLoaded %d command(s)...", cmdCount)
			fmt.Println("\nType help for available commands")
			fmt.Print("\n")
		}
	} else {
		fmt.Print("\n\nNo commands found")
	}

	go shell.Open(bufio.NewReader(os.Stdin))

	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT)
	select {
	case <-sigs:
		cancel()
		<-shell.Closed()
	case <-shell.Closed():
	}
}

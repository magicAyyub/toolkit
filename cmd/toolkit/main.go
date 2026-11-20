package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"toolkit/internal/runpod"
	"toolkit/internal/workshop"
)

const usage = `Usage:
  toolkit workshop open <name> [flags]   open a GPU machine and connect to it
  toolkit workshop connect <name>        reconnect to an open workshop
  toolkit workshop list                  list open workshops
  toolkit workshop close <name> [--yes]  delete a workshop

"ws" is a short alias for "workshop". Run "toolkit workshop open -h" for open's flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 || (args[0] != "workshop" && args[0] != "ws") {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	command, args := args[1], args[2:]
	ctx := context.Background()

	config, err := readConfig()
	if err != nil {
		return err
	}

	switch command {
	case "open":
		return openWorkshop(ctx, config, args)
	case "connect":
		flags := flag.NewFlagSet("connect", flag.ExitOnError)
		sshKey := flags.String("ssh-key", config.sshKey(), "SSH public key")
		name := parseWithName(flags, args)
		client, err := config.newClient()
		if err != nil {
			return err
		}
		return workshop.Connect(ctx, client, name, expandHome(*sshKey))
	case "list":
		client, err := config.newClient()
		if err != nil {
			return err
		}
		return workshop.List(ctx, client)
	case "close":
		flags := flag.NewFlagSet("close", flag.ExitOnError)
		yes := flags.Bool("yes", false, "don't ask for confirmation")
		name := parseWithName(flags, args)
		client, err := config.newClient()
		if err != nil {
			return err
		}
		return workshop.Close(ctx, client, name, *yes)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

func openWorkshop(ctx context.Context, config configFile, args []string) error {
	flags := flag.NewFlagSet("open", flag.ExitOnError)
	gpu := flags.String("gpu", "RTX 4090", "GPU type, as named in the RunPod catalog")
	maxPrice := flags.Float64("max-price", 1.00, "maximum price in USD per hour")
	budget := flags.String("budget", "$3", `budget as a duration ("2h", "90m") or dollars ("$5")`)
	idle := flags.Duration("idle", 30*time.Minute, "delete after this long with no SSH session and GPU at rest")
	community := flags.Bool("community", false, "use the community cloud (cheaper, less reliable)")
	debug := flags.Bool("debug", false, "print each SSH probe command with its exit code and output")
	sshKey := flags.String("ssh-key", config.sshKey(), "SSH public key installed on the machine")
	name := parseWithName(flags, args)

	parsedBudget, err := workshop.ParseBudget(*budget)
	if err != nil {
		return err
	}
	client, err := config.newClient()
	if err != nil {
		return err
	}
	return workshop.Open(ctx, client, workshop.OpenOptions{
		Name:        name,
		GPU:         *gpu,
		MaxPrice:    *maxPrice,
		Budget:      parsedBudget,
		IdleTimeout: *idle,
		Community:   *community,
		Debug:       *debug,
		SSHKey:      expandHome(*sshKey),
	})
}

// parseWithName parses flags placed before or after the single positional
// <name> argument; the standard flag package stops at the first positional.
func parseWithName(flags *flag.FlagSet, args []string) string {
	flags.Parse(args)
	if flags.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "%s: missing workshop name\n", flags.Name())
		os.Exit(2)
	}
	name := flags.Arg(0)
	flags.Parse(flags.Args()[1:])
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s: unexpected arguments: %s\n", flags.Name(), strings.Join(flags.Args(), " "))
		os.Exit(2)
	}
	return name
}

// configFile holds the "key = value" settings from ~/.config/toolkit/config.
// Known keys: runpod_api_key, ssh_key.
type configFile struct {
	path     string
	settings map[string]string
}

func readConfig() (configFile, error) {
	cfg := configFile{path: filepath.Join(workshop.ConfigDir(), "config"), settings: map[string]string{}}
	file, err := os.Open(cfg.path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return cfg, fmt.Errorf("%s: expected \"key = value\", got %q", cfg.path, line)
		}
		cfg.settings[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return cfg, fmt.Errorf("reading %s: %w", cfg.path, err)
	}
	return cfg, nil
}

// newClient uses RUNPOD_API_KEY, or runpod_api_key from the config file.
func (c configFile) newClient() (*runpod.Client, error) {
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		return runpod.NewClient(key), nil
	}
	if key := c.settings["runpod_api_key"]; key != "" {
		return runpod.NewClient(key), nil
	}
	return nil, fmt.Errorf("no RunPod API key: set RUNPOD_API_KEY or add \"runpod_api_key = ...\" to %s", c.path)
}

// sshKey is the default for --ssh-key: ssh_key from the config file, or ~/.ssh/id_ed25519.pub.
func (c configFile) sshKey() string {
	if key := c.settings["ssh_key"]; key != "" {
		return key
	}
	return "~/.ssh/id_ed25519.pub"
}

// expandHome expands a leading "~/", which the shell doesn't do inside the
// config file or in "--ssh-key=~/..." forms.
func expandHome(path string) string {
	if rest, found := strings.CutPrefix(path, "~/"); found {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return path
}

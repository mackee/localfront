// Command localfront runs a local Amazon CloudFront emulator driven by
// CloudFormation templates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const rootUsage = `localfront is a local Amazon CloudFront emulator driven by CloudFormation templates.

Usage:
  localfront serve [flags]

Run "localfront serve -h" for the flags of the serve subcommand.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, rootUsage)
		return 2
	}
	switch args[0] {
	case "serve":
		if err := runServe(ctx, args[1:], stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 2
			}
			fmt.Fprintf(stderr, "localfront: error: %v\n", err)
			return 1
		}
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stderr, rootUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "localfront: unknown subcommand %q\n\n%s", args[0], rootUsage)
		return 2
	}
}

type serveOptions struct {
	templates  []string
	listen     string
	s3Endpoint string
	s3Region   string
	s3Access   string
	s3Secret   string
	parameters map[string]string
	kvsSeeds   map[string]string
	logLevel   string
}

func parseServeFlags(args []string, stderr io.Writer) (*serveOptions, error) {
	opts := &serveOptions{
		parameters: map[string]string{},
		kvsSeeds:   map[string]string{},
	}
	fs := flag.NewFlagSet("localfront serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Var((*repeatedString)(&opts.templates), "template", "CloudFormation template file (JSON or YAML); repeatable")
	fs.StringVar(&opts.listen, "listen", ":8080", "address for the data plane to listen on")
	fs.StringVar(&opts.s3Endpoint, "s3-endpoint", "", "endpoint URL of the S3-compatible object store backing S3 origins")
	fs.StringVar(&opts.s3Region, "s3-region", "us-east-1", "region used to sign requests to the object store")
	fs.StringVar(&opts.s3Access, "s3-access-key", "", "access key for the object store")
	fs.StringVar(&opts.s3Secret, "s3-secret-key", "", "secret key for the object store")
	fs.Var(keyValueFlag(opts.parameters), "parameter", "template parameter override as key=value; repeatable")
	fs.Var(keyValueFlag(opts.kvsSeeds), "kvs-seed", "KeyValueStore seed as <store>=<file.json>; repeatable")
	fs.StringVar(&opts.logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if len(opts.templates) == 0 {
		return nil, errors.New("at least one --template is required")
	}
	return opts, nil
}

func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	opts, err := parseServeFlags(args, stderr)
	if err != nil {
		return err
	}
	logger, err := newLogger(stderr, opts.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	return serve(ctx, opts, logger)
}

func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: must be one of debug, info, warn, error", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})), nil
}

// repeatedString collects repeated occurrences of a string flag.
type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// keyValueFlag collects repeated key=value flags into a map.
type keyValueFlag map[string]string

func (m keyValueFlag) String() string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, ",")
}

func (m keyValueFlag) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	if _, dup := m[key]; dup {
		return fmt.Errorf("duplicate key %q", key)
	}
	m[key] = value
	return nil
}

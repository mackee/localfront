// Command localfront runs a local Amazon CloudFront emulator driven by
// CloudFormation templates.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
)

const rootDescription = "localfront is a local Amazon CloudFront emulator driven by CloudFormation templates."

// cli is the kong-parsed command-line interface.
type cli struct {
	Serve serveCmd `cmd:"" help:"Run the local CloudFront data plane from CloudFormation templates."`
}

// serveCmd is the flag set for "localfront serve".
type serveCmd struct {
	Templates  []string          `name:"template" required:"" placeholder:"FILE" help:"CloudFormation template file (JSON or YAML); repeatable."`
	Listen     string            `name:"listen" default:":8080" placeholder:"ADDR" help:"Address for the data plane to listen on."`
	PublicHost string            `name:"public-host" required:"" env:"LOCALFRONT_PUBLIC_HOST" placeholder:"HOST[:PORT]" help:"Host (optionally host:port) localfront is reached at; canned signed URLs are verified against it verbatim."`
	S3Endpoint string            `name:"s3-endpoint" placeholder:"URL" help:"Endpoint URL of the S3-compatible object store backing S3 origins."`
	S3Region   string            `name:"s3-region" default:"us-east-1" placeholder:"REGION" help:"Region used to sign requests to the object store."`
	S3Access   string            `name:"s3-access-key" placeholder:"KEY" help:"Access key for the object store."`
	S3Secret   string            `name:"s3-secret-key" placeholder:"KEY" help:"Secret key for the object store."`
	Parameter  map[string]string `name:"parameter" placeholder:"KEY=VALUE" help:"Template parameter override; repeatable."`
	KVSSeed    map[string]string `name:"kvs-seed" placeholder:"STORE=FILE" help:"KeyValueStore seed as <store>=<file.json>; repeatable."`
	LogLevel   string            `name:"log-level" default:"info" enum:"debug,info,warn,error" placeholder:"LEVEL" help:"Log level (debug, info, warn, error)."`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var c cli
	kctx := kong.Parse(&c,
		kong.Name("localfront"),
		kong.Description(rootDescription),
		kong.UsageOnError(),
	)

	switch kctx.Command() {
	case "serve":
		if err := runServe(ctx, &c.Serve, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "localfront: error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "localfront: unknown command %q\n", kctx.Command())
		os.Exit(2)
	}
}

func runServe(ctx context.Context, c *serveCmd, stderr io.Writer) error {
	opts := c.toOptions()
	logger, err := newLogger(stderr, opts.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	return serve(ctx, opts, logger)
}

// serveOptions is the resolved configuration the serve pipeline and the
// reloader consume. The kong command struct is mapped onto it so the rest of
// the package stays decoupled from the parser.
type serveOptions struct {
	templates  []string
	listen     string
	publicHost string
	s3Endpoint string
	s3Region   string
	s3Access   string
	s3Secret   string
	parameters map[string]string
	kvsSeeds   map[string]string
	logLevel   string
}

func (c *serveCmd) toOptions() *serveOptions {
	parameters := c.Parameter
	if parameters == nil {
		parameters = map[string]string{}
	}
	kvsSeeds := c.KVSSeed
	if kvsSeeds == nil {
		kvsSeeds = map[string]string{}
	}
	return &serveOptions{
		templates:  c.Templates,
		listen:     c.Listen,
		publicHost: c.PublicHost,
		s3Endpoint: c.S3Endpoint,
		s3Region:   c.S3Region,
		s3Access:   c.S3Access,
		s3Secret:   c.S3Secret,
		parameters: parameters,
		kvsSeeds:   kvsSeeds,
		logLevel:   c.logLevel(),
	}
}

// logLevel returns the configured log level, defaulting to info when unset
// (the zero value reached through code paths that bypass kong defaults).
func (c *serveCmd) logLevel() string {
	if c.LogLevel == "" {
		return "info"
	}
	return c.LogLevel
}

func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: must be one of debug, info, warn, error", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})), nil
}

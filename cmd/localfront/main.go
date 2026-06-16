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

// version is the build identifier. It is "dev" for `go build` / `go install`
// and is overridden via -ldflags="-X main.version=..." in release builds
// (see Dockerfile and .github/workflows/release.yml).
var version = "dev"

// cli is the kong-parsed command-line interface.
type cli struct {
	Serve   serveCmd         `cmd:"" help:"Run the local CloudFront data plane from CloudFormation templates."`
	Version kong.VersionFlag `name:"version" short:"V" help:"Print the localfront version and exit."`
}

// serveCmd is the flag set for "localfront serve". Every flag also accepts a
// LOCALFRONT_* environment variable so the binary can be fully configured for
// container deployments without an entrypoint wrapper. Repeatable flags
// (template, parameter, kvs-seed) use comma-separated entries when set via
// env (kong's default mapper splits on commas, and map entries are KEY=VALUE).
type serveCmd struct {
	Templates  []string          `name:"template" required:"" env:"LOCALFRONT_TEMPLATE" placeholder:"FILE" help:"CloudFormation template file (JSON or YAML); repeatable. Env: comma-separated list."`
	Listen     string            `name:"listen" default:":8080" env:"LOCALFRONT_LISTEN" placeholder:"ADDR" help:"Address for the data plane to listen on."`
	PublicHost string            `name:"public-host" required:"" env:"LOCALFRONT_PUBLIC_HOST" placeholder:"HOST[:PORT]" help:"Host (optionally host:port) localfront is reached at; canned signed URLs are verified against it verbatim."`
	S3Endpoint string            `name:"s3-endpoint" env:"LOCALFRONT_S3_ENDPOINT" placeholder:"URL" help:"Endpoint URL of the S3-compatible object store backing S3 origins."`
	S3Region   string            `name:"s3-region" default:"us-east-1" env:"LOCALFRONT_S3_REGION" placeholder:"REGION" help:"Region used to sign requests to the object store."`
	S3Access   string            `name:"s3-access-key" env:"LOCALFRONT_S3_ACCESS_KEY" placeholder:"KEY" help:"Access key for the object store."`
	S3Secret   string            `name:"s3-secret-key" env:"LOCALFRONT_S3_SECRET_KEY" placeholder:"KEY" help:"Secret key for the object store."`
	Parameter  map[string]string `name:"parameter" env:"LOCALFRONT_PARAMETER" placeholder:"KEY=VALUE" help:"Template parameter override; repeatable. Env: comma-separated KEY=VALUE pairs."`
	KVSSeed    map[string]string `name:"kvs-seed" env:"LOCALFRONT_KVS_SEED" placeholder:"STORE=FILE" help:"KeyValueStore seed as <store>=<file.json>; repeatable. Env: comma-separated STORE=FILE pairs."`
	LogLevel   string            `name:"log-level" default:"info" env:"LOCALFRONT_LOG_LEVEL" enum:"debug,info,warn,error" placeholder:"LEVEL" help:"Log level (debug, info, warn, error)."`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var c cli
	kctx := kong.Parse(&c,
		kong.Name("localfront"),
		kong.Description(rootDescription),
		kong.UsageOnError(),
		kong.Vars{"version": version},
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

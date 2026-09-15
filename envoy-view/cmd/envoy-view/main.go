// Command envoy-view fetches an Envoy config dump and renders it as a graph of
// filter chain pipelines.
//
// By default it serves a web UI. With -print it renders once to stdout instead,
// which is handy in a terminal and is how the resolution rules are eyeballed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/boweidu/envoy-view/internal/admin"
	"github.com/boweidu/envoy-view/internal/api"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/server"
	"github.com/boweidu/envoy-view/internal/snapshot"
	"github.com/boweidu/envoy-view/internal/webui"
	"github.com/boweidu/envoy-view/internal/xds"
)

type options struct {
	adminAddr  string
	dumpFile   string
	includeEDS bool
	listenAddr string
	print      string
	root       string
	verbose    bool

	snapshotTTL  time.Duration
	maxSnapshots int
}

func main() {
	var opt options
	flag.StringVar(&opt.adminAddr, "envoy-admin", "127.0.0.1:9901", "address of the Envoy admin interface")
	flag.StringVar(&opt.dumpFile, "dump-file", "", "read a saved config_dump JSON file instead of querying Envoy")
	flag.BoolVar(&opt.includeEDS, "include-eds", true, "ask Envoy to include endpoint (EDS) data in the dump")
	flag.StringVar(&opt.listenAddr, "addr", "127.0.0.1:8080", "address to serve the web UI on")
	flag.StringVar(&opt.print, "print", "", "render once to stdout and exit: summary or graph")
	flag.StringVar(&opt.root, "root", "", "with -print graph, expand only this listener")
	flag.BoolVar(&opt.verbose, "v", false, "verbose logging")
	flag.DurationVar(&opt.snapshotTTL, "snapshot-ttl", 30*time.Minute, "how long a fetched snapshot stays browsable")
	flag.IntVar(&opt.maxSnapshots, "max-snapshots", 16, "how many snapshots to keep in memory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opt); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "envoy-view: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opt options) error {
	level := slog.LevelInfo
	if opt.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if opt.print != "" {
		return printOnce(ctx, opt)
	}
	return serve(ctx, opt, log)
}

// ------------------------------------------------------------------- serving

func serve(ctx context.Context, opt options, log *slog.Logger) error {
	store := snapshot.NewStore(opt.maxSnapshots, opt.snapshotTTL)

	apiSrv := &api.Server{Store: store, Log: log, IncludeEDS: opt.includeEDS}
	if opt.dumpFile == "" {
		client, err := admin.New(opt.adminAddr)
		if err != nil {
			return err
		}
		apiSrv.Client = client
	}

	// A file given on the command line is preloaded so the UI opens straight
	// onto it rather than making the user upload it again.
	if opt.dumpFile != "" {
		if err := preload(store, opt.dumpFile); err != nil {
			return err
		}
	}

	ui, err := webui.FS()
	if err != nil {
		return fmt.Errorf("load embedded web UI: %w", err)
	}

	httpSrv := &http.Server{
		Handler:           server.New(apiSrv.Routes(), ui, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", opt.listenAddr)
	if err != nil {
		return err
	}
	source := opt.dumpFile
	if source == "" {
		source = opt.adminAddr
	}
	log.Info("serving", "url", "http://"+ln.Addr().String(), "source", source)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func preload(store *snapshot.Store, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	index, err := xds.Parse(raw)
	if err != nil {
		return err
	}
	_, err = store.Put(&snapshot.Snapshot{Source: path, SizeBytes: len(raw), Index: index})
	return err
}

// ------------------------------------------------------------------ printing

func printOnce(ctx context.Context, opt options) error {
	raw, source, err := loadDump(ctx, opt)
	if err != nil {
		return err
	}
	index, err := xds.Parse(raw)
	if err != nil {
		return err
	}

	switch opt.print {
	case "summary":
		return summarize(os.Stdout, source, len(raw), index)
	case "graph":
		gopts := graph.Options{IncludeOrphanClusters: opt.root == ""}
		if opt.root != "" {
			gopts.Roots = []string{opt.root}
		}
		return graph.Build(index, gopts).WriteText(os.Stdout)
	default:
		return fmt.Errorf("unknown -print %q: want summary or graph", opt.print)
	}
}

func loadDump(ctx context.Context, opt options) (raw []byte, source string, err error) {
	if opt.dumpFile != "" {
		raw, err = os.ReadFile(opt.dumpFile)
		if err != nil {
			return nil, "", err
		}
		return raw, opt.dumpFile, nil
	}

	client, err := admin.New(opt.adminAddr)
	if err != nil {
		return nil, "", err
	}
	raw, err = client.ConfigDump(ctx, opt.includeEDS)
	if err != nil {
		return nil, "", err
	}
	return raw, client.Addr(), nil
}

func summarize(out *os.File, source string, size int, index *xds.Index) error {
	fmt.Fprintf(out, "source: %s (%.1f KiB)\n\n", source, float64(size)/1024)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tCOUNT")
	counts := index.Counts()
	for _, k := range xds.Kinds {
		fmt.Fprintf(tw, "%s\t%d\n", k, counts[k])
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if warns := index.Warnings(); len(warns) > 0 {
		fmt.Fprintf(out, "\n%d warning(s):\n", len(warns))
		for _, w := range warns {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	return nil
}

// Command voidmcp is the voidmcp CLI. It can run an MCP server (HTTP or
// stdio) and manage registered MCP servers from the command line.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/registry"
	"github.com/voidmind-io/voidmcp/internal/server"
	"github.com/voidmind-io/voidmcp/internal/store"
)

// Version is the build version, overridden at build time via -ldflags.
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "add":
		runAdd()
	case "remove":
		runRemove()
	case "list":
		runList()
	case "version":
		fmt.Println("voidmcp " + Version)
	default:
		printUsage()
		os.Exit(1)
	}
}

// runServe starts the MCP server in either HTTP or stdio mode.
func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "Bind address (use 0.0.0.0 to expose on network)")
	port := fs.Int("port", 8090, "HTTP port (ignored when --stdio is set)")
	stdio := fs.Bool("stdio", false, "Use stdio transport instead of HTTP")
	noAuth := fs.Bool("no-auth", false, "Disable bearer token auth (use when behind a trusted reverse proxy)")
	tokenFlag := fs.String("token", "", "Bearer token to use for HTTP auth (default: auto-generate and persist)")
	dbPath := fs.String("db", defaultDBPath(), "Database path")
	poolSize := fs.Int("pool-size", 4, "WASM runtime pool size")
	memLimit := fs.Int("memory", 16, "Per-execution memory limit in MB")
	timeout := fs.Duration("timeout", 30*time.Second, "Per-execution timeout")
	maxToolCalls := fs.Int("max-tool-calls", 50, "Maximum tool calls per execution")
	schemaTTL := fs.Duration("schema-ttl", 168*time.Hour, "How often to re-infer output schemas (default: 7 days)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		os.Exit(1)
	}

	pool, err := executor.NewPool(*poolSize, *memLimit, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create executor pool: %v\n", err)
		st.Close()
		os.Exit(1)
	}

	exec := executor.New(pool)

	reg := registry.New(st, time.Hour)
	if err := reg.Load(context.Background()); err != nil {
		// Non-fatal: individual server errors are logged inside Load.
		log.Printf("warning: registry load: %v", err)
	}

	var bearerToken string
	if !*stdio && !*noAuth {
		// Token resolution order: --token flag > VOIDMCP_TOKEN env > persisted > generate new.
		envToken := os.Getenv("VOIDMCP_TOKEN")
		switch {
		case *tokenFlag != "":
			if len(*tokenFlag) < 32 {
				fmt.Fprintln(os.Stderr, "error: --token must be at least 32 characters")
				os.Exit(1)
			}
			bearerToken = *tokenFlag
		case envToken != "":
			if len(envToken) < 32 {
				fmt.Fprintln(os.Stderr, "error: VOIDMCP_TOKEN must be at least 32 characters")
				os.Exit(1)
			}
			bearerToken = envToken
		default:
			// Try to load a previously persisted token; generate and persist one
			// if none exists yet.
			tok, getErr := st.GetSetting(context.Background(), "bearer_token")
			if getErr == nil {
				bearerToken = tok
			} else {
				tok, err = server.GenerateToken()
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: generate auth token: %v\n", err)
					st.Close()
					os.Exit(1)
				}
				if err = st.SetSetting(context.Background(), "bearer_token", tok); err != nil {
					fmt.Fprintf(os.Stderr, "error: persist auth token: %v\n", err)
					st.Close()
					os.Exit(1)
				}
				bearerToken = tok
			}
		}
	}

	srv := server.New(reg, exec, st, server.Config{
		
		PoolSize:        *poolSize,
		MemoryLimitMB:   *memLimit,
		Timeout:         *timeout,
		MaxToolCalls:    *maxToolCalls,
		BearerToken:     bearerToken,
		SchemaTTL:       *schemaTTL,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *stdio {
		srv.ServeStdio(ctx)
	} else {
		addr := fmt.Sprintf("%s:%d", *host, *port)
		httpServer := &http.Server{
			Addr:    addr,
			Handler: srv,
		}
		go func() {
			<-ctx.Done()
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			_ = httpServer.Shutdown(shutCtx)
		}()
		log.Printf("voidmcp listening on %s", addr)
		if bearerToken != "" {
			log.Printf("Authorization token: Bearer %s...%s", bearerToken[:8], bearerToken[len(bearerToken)-4:])
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}

	reg.Close()
	pool.Close()
	if err := st.Close(); err != nil {
		log.Printf("store close: %v", err)
	}
}

// runAdd registers a new MCP server from the command line.
func runAdd() {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	token := fs.String("token", "", "Bearer token for authentication (HTTP only)")
	header := fs.String("header", "", "Custom auth header name (HTTP only)")
	dbPath := fs.String("db", defaultDBPath(), "Database path")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	args := fs.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: voidmcp add <name> <url-or-command> [--token X] [--header X]")
		os.Exit(1)
	}

	name, target := args[0], args[1]

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	reg := registry.New(st, time.Hour)

	srv := store.MCPServer{Name: name}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		srv.URL = target
		if *token != "" {
			srv.AuthType = "bearer"
			srv.AuthToken = *token
			if *header != "" {
				srv.AuthType = "header"
				srv.AuthHeader = *header
			}
		}
	} else {
		srv.Command = target
	}

	tools, err := reg.Add(context.Background(), srv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Added %q with %d tools:\n", name, len(tools))
	for _, t := range tools {
		fmt.Printf("  - %s: %s\n", t.Name, t.Description)
	}
}

// runRemove unregisters an MCP server from the command line.
func runRemove() {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Database path")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	args := fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: voidmcp remove <name>")
		os.Exit(1)
	}
	name := args[0]

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	reg := registry.New(st, time.Hour)

	if err := reg.Remove(context.Background(), name); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %q.\n", name)
}

// runList prints all registered MCP servers in a table.
func runList() {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Database path")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	reg := registry.New(st, time.Hour)
	if err := reg.Load(context.Background()); err != nil {
		log.Printf("warning: registry load: %v", err)
	}

	servers := reg.List()
	if len(servers) == 0 {
		fmt.Println("No servers registered.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tENDPOINT\tSTATUS\tTOOLS")
	fmt.Fprintln(w, "----\t--------\t------\t-----")
	for _, srv := range servers {
		endpoint := srv.URL
		if endpoint == "" {
			endpoint = srv.Command
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", srv.Name, endpoint, srv.Status, len(srv.Tools))
	}
	w.Flush()
}

// defaultDBPath returns ~/.voidmcp/voidmcp.db, falling back to
// ./.voidmcp/voidmcp.db if the home directory cannot be determined.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".voidmcp", "voidmcp.db")
	}
	return filepath.Join(home, ".voidmcp", "voidmcp.db")
}

// printUsage prints a brief command reference to stderr.
func printUsage() {
	fmt.Fprintln(os.Stderr, `voidmcp - MCP server with WASM tool execution

Usage:
  voidmcp serve   [--host ADDR] [--port N] [--stdio] [--no-auth] [--token T] [--db PATH]
                  [--pool-size N] [--memory MB] [--timeout D]
                  [--max-tool-calls N] [--schema-ttl D]
  voidmcp add     <name> <url-or-command> [--token T] [--header H] [--db PATH]
  voidmcp remove  <name> [--db PATH]
  voidmcp list    [--db PATH]
  voidmcp version`)
}

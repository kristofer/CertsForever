// Command certsforever runs the certificate server and its admin CLI.
//
//	certsforever serve                         run the HTTP server
//	certsforever course -slug java -title "Java Full-Stack Developer" -skills "Java,Spring,SQL"
//	certsforever import cohort.csv [-out links.csv]
//	certsforever revoke -reason "issued in error" ZCW-XXXXXXXXXX
//	certsforever claim-link ZCW-XXXXXXXXXX
//	certsforever demo                          seed a sample public certificate
//
// Operations:
//
//	certsforever migrate                       apply pending migrations and exit
//	certsforever backup DEST.db                consistent online backup (VACUUM INTO)
//	certsforever restore SRC.db                replace the database (server must be stopped)
//	certsforever healthcheck                   exit 0 if the local server is ready
//	certsforever version
//
// Configuration comes from CERTS_* environment variables (see README).
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"certsforever/internal/buildinfo"
	"certsforever/internal/config"
	"certsforever/internal/importer"
	"certsforever/internal/store"
	"certsforever/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]

	// Commands that must work even with incomplete configuration.
	switch cmd {
	case "version":
		fmt.Println("certsforever", buildinfo.Get())
		return
	case "healthcheck":
		exit(healthcheck(ctx, config.FromEnv()))
		return
	case "-h", "--help", "help":
		usage()
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:\n "+strings.ReplaceAll(err.Error(), "\n", "\n "))
		os.Exit(1)
	}

	switch cmd {
	case "serve":
		err = serve(ctx, cfg)
	case "migrate":
		err = migrate(ctx, cfg)
	case "backup":
		err = backup(ctx, cfg, args)
	case "restore":
		err = restore(ctx, cfg, args)
	case "course":
		err = course(ctx, cfg, args)
	case "import":
		err = importCSV(ctx, cfg, args)
	case "revoke":
		err = revoke(ctx, cfg, args)
	case "claim-link":
		err = claimLink(ctx, cfg, args)
	case "demo":
		err = demo(ctx, cfg)
	default:
		usage()
		os.Exit(2)
	}
	exit(err)
}

func exit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: certsforever <command> [flags]

commands:
  serve        run the HTTP server
  course       create or update a course
  import       issue certificates from a cohort CSV
  revoke       revoke a certificate
  claim-link   issue a new claim link for a certificate
  demo         seed a sample public certificate for local testing

operations:
  migrate      apply pending database migrations and exit
  backup       write a consistent backup:   backup /data/backups/certs-2026-10-06.db
  restore      replace the database from a backup (stop the server first)
  healthcheck  exit 0 if the server on CERTS_ADDR is ready (for Docker)
  version      print the build version`)
}

func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if m := st.Migration; len(m.Applied) > 0 && log != nil {
		log.Info("database migrated", "applied", m.Applied, "schema_version", m.Version, "snapshot", m.Snapshot)
	}
	return st, nil
}

func serve(ctx context.Context, cfg config.Config) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	info := buildinfo.Get()
	log.Info("starting", "version", info.Version, "commit", info.Commit, "go", info.Go, "env", cfg.Env)

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := web.New(cfg, st, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if cfg.AdminToken == "" {
		log.Warn("CERTS_ADMIN_TOKEN not set; admin API disabled")
	}

	// Keep the query planner's statistics fresh on long-running servers.
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.Optimize(ctx); err != nil {
					log.Warn("optimize", "err", err)
				}
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "db", cfg.DBPath,
			"schema_version", st.Migration.Version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func migrate(ctx context.Context, cfg config.Config) error {
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	m := st.Migration
	if len(m.Applied) == 0 {
		fmt.Printf("schema is current (version %d)\n", m.Version)
		return nil
	}
	fmt.Printf("applied migrations %v; schema version %d\n", m.Applied, m.Version)
	if m.Snapshot != "" {
		fmt.Println("pre-migration snapshot:", m.Snapshot)
	}
	return nil
}

func backup(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: certsforever backup DEST.db")
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	start := time.Now()
	if err := st.Backup(ctx, args[0]); err != nil {
		return err
	}
	fi, err := os.Stat(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s (%d bytes, %s)\n", args[0], fi.Size(), time.Since(start).Round(time.Millisecond))
	return nil
}

func restore(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	yes := fs.Bool("yes", false, "confirm: the server is stopped and the current database may be replaced")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever restore -yes SRC.db   (stop the server first)")
	}
	if !*yes {
		return errors.New("refusing to restore without -yes; stop the server first, then re-run with -yes")
	}
	if ready(ctx, cfg) == nil {
		return errors.New("a server is still answering on CERTS_ADDR; stop it before restoring")
	}
	kept, err := store.Restore(ctx, fs.Arg(0), cfg.DBPath)
	if err != nil {
		return err
	}
	fmt.Printf("restored %s from %s\n", cfg.DBPath, fs.Arg(0))
	if kept != "" {
		fmt.Println("previous database kept at", kept)
	}
	return nil
}

func healthcheck(ctx context.Context, cfg config.Config) error {
	return ready(ctx, cfg)
}

// ready asks the server on cfg.Addr for /readyz.
func ready(ctx context.Context, cfg config.Config) error {
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %s", resp.Status)
	}
	return nil
}

func course(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("course", flag.ExitOnError)
	slug := fs.String("slug", "", "URL-safe identifier, e.g. java")
	title := fs.String("title", "", "title printed on the certificate")
	desc := fs.String("description", "", "short description")
	skills := fs.String("skills", "", "comma-separated skills")
	hours := fs.Int("hours", 0, "instructional hours")
	fs.Parse(args)

	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	id, err := st.CreateCourse(ctx, store.Course{
		Slug: *slug, Title: *title, Description: *desc,
		Skills: strings.Split(*skills, ","), Hours: *hours,
	})
	if err != nil {
		return err
	}
	fmt.Printf("course %q saved (id %d)\n", *slug, id)
	return nil
}

func importCSV(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	out := fs.String("out", "", "write email/claim links CSV here (default stdout)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever import [-out links.csv] cohort.csv")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	reqs, err := importer.Parse(f)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	results, err := st.Issue(ctx, reqs)
	if err != nil {
		return err
	}

	var w io.Writer = os.Stdout
	if *out != "" {
		of, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // contains claim links
		if err != nil {
			return err
		}
		defer of.Close()
		w = of
	}
	cw := csv.NewWriter(w)
	cw.Write([]string{"email", "full_name", "certificate_id", "certificate_url", "claim_url", "status"})
	issued := 0
	for _, r := range results {
		status, claim := "existing", ""
		if !r.Existing {
			status, claim = "issued", web.ClaimURL(cfg.BaseURL, r.ClaimToken)
			issued++
		}
		cw.Write([]string{r.Email, r.FullName, r.CertificateID, cfg.BaseURL + "/c/" + r.CertificateID, claim, status})
	}
	cw.Flush()
	fmt.Fprintf(os.Stderr, "%d issued, %d already existed\n", issued, len(results)-issued)
	return cw.Error()
}

func revoke(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	reason := fs.String("reason", "", "reason (stored, not shown publicly)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever revoke [-reason text] CERT_ID")
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Revoke(ctx, fs.Arg(0), *reason); err != nil {
		return err
	}
	fmt.Println("revoked", fs.Arg(0))
	return nil
}

func claimLink(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: certsforever claim-link CERT_ID")
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	token, err := st.NewClaimLink(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Println(web.ClaimURL(cfg.BaseURL, token))
	return nil
}

func demo(ctx context.Context, cfg config.Config) error {
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.CreateCourse(ctx, store.Course{
		Slug:   "java-fullstack",
		Title:  "Java Full-Stack Developer",
		Skills: []string{"Java", "Spring Boot", "SQL", "REST APIs", "Git", "Agile"},
		Hours:  480,
	}); err != nil {
		return err
	}
	res, err := st.Issue(ctx, []store.IssueRequest{{
		Email: "demo.student@example.com", FullName: "Grace Hopper",
		CourseSlug: "java-fullstack", Cohort: "Java 13",
		CompletedOn: time.Now().UTC().Truncate(24 * time.Hour),
	}})
	if err != nil {
		return err
	}
	r := res[0]
	if err := st.SetVisibility(ctx, r.CertificateID, "public"); err != nil {
		return err
	}
	token := r.ClaimToken
	if token == "" {
		if token, err = st.NewClaimLink(ctx, r.CertificateID); err != nil {
			return err
		}
	}
	fmt.Println("public page: ", cfg.BaseURL+"/c/"+r.CertificateID)
	fmt.Println("share image: ", cfg.BaseURL+"/c/"+r.CertificateID+"/og.png")
	fmt.Println("claim page:  ", web.ClaimURL(cfg.BaseURL, token))
	return nil
}

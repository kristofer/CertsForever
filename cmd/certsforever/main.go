// Command certsforever runs the certificate server and its admin CLI.
//
//	certsforever serve                         run the HTTP server
//	certsforever client add -slug zcw -name "Zip Code Wilmington" -prefix ZCW -site https://zipcodewilmington.com
//	certsforever client list | update | suspend | activate
//	certsforever course -client zcw -slug java -title "Java Full-Stack Developer" -skills "Java,Spring,SQL"
//	certsforever import -client zcw cohort.csv [-out links.csv]
//	certsforever revoke -reason "issued in error" ZCW-XXXXXXXXXX
//	certsforever claim-link ZCW-XXXXXXXXXX
//	certsforever demo                          seed a sample public certificate
//
// -client may be omitted when CERTS_CLIENT is set or only one client exists;
// revoke and claim-link infer it from the certificate ID's prefix.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"certsforever/internal/buildinfo"
	"certsforever/internal/config"
	"certsforever/internal/emails"
	"certsforever/internal/importer"
	"certsforever/internal/mail"
	"certsforever/internal/outbox"
	"certsforever/internal/secretbox"
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
	case "client":
		err = clientCmd(ctx, cfg, args)
	case "superadmin":
		err = superadminCmd(ctx, cfg, args)
	case "login-link":
		err = loginLinkCmd(ctx, cfg, args)
	case "user":
		err = userCmd(ctx, cfg, args)
	case "email":
		err = emailCmd(ctx, cfg, args)
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
  client       manage clients: add, update, list, suspend, activate
  superadmin   manage platform administrators: add, list, remove
  login-link   print a one-time sign-in link for a user (when email isn't set up)
  user         user maintenance: reset-2fa, disable, enable
  email        email queue: status, retry, test, suppress, unsuppress
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
	if retired := config.RetiredInUse(); len(retired) > 0 {
		log.Warn("ignoring retired environment variables; branding now lives on each client "+
			"(see `certsforever client update`)", "vars", retired)
	}

	st, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Close()
	mailer, mode, err := newMailer(cfg, log)
	if err != nil {
		return err
	}
	if cfg.DevMasterKey {
		log.Warn("CERTS_MASTER_KEY not set: using the public development key (production refuses to start without one)")
	}
	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		return err
	}

	// Email: a durable queue in the database, delivered by a background worker.
	var queue *outbox.Queue
	workerDone := make(chan struct{})
	if mailer != nil {
		queue = outbox.NewQueue(st, box)
		w := &outbox.Worker{Queue: queue, Sender: mailer, Log: log,
			Platform: emails.Platform{Name: cfg.PlatformName, BaseURL: cfg.BaseURL}}
		go func() { defer close(workerDone); w.Run(ctx) }()
		log.Info("email worker started", "delivery", mode)
	} else {
		close(workerDone)
	}

	s, err := web.New(cfg, st, log, web.Deps{Queue: queue, EmailMode: mode})
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
				if err := st.PruneAuth(ctx); err != nil {
					log.Warn("prune sessions", "err", err)
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
		err := srv.Shutdown(shutdownCtx)
		// Let an in-flight email finish before the database closes; it's
		// re-sent after restart if it doesn't (at-least-once).
		select {
		case <-workerDone:
		case <-time.After(10 * time.Second):
			log.Warn("email worker still busy at shutdown")
		}
		return err
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
	client := fs.String("client", os.Getenv("CERTS_CLIENT"), "client slug")
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
	sc, err := resolveScope(ctx, st, *client)
	if err != nil {
		return err
	}
	id, err := st.CreateCourse(ctx, sc, store.Course{
		Slug: *slug, Title: *title, Description: *desc,
		Skills: strings.Split(*skills, ","), Hours: *hours,
	})
	if err != nil {
		return err
	}
	auditCLI(ctx, st, sc, "course.save", "course", *slug, map[string]any{"title": *title})
	fmt.Printf("course %q saved for client %q (id %d)\n", *slug, sc.Client().Slug, id)
	return nil
}

func importCSV(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	client := fs.String("client", os.Getenv("CERTS_CLIENT"), "client slug")
	out := fs.String("out", "", "write email/claim links CSV here (default stdout)")
	sendEmail := fs.Bool("email", false, "email each new student their claim link (queued; the server delivers it)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever import [-client slug] [-email] [-out links.csv] cohort.csv")
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
	sc, err := resolveScope(ctx, st, *client)
	if err != nil {
		return err
	}
	var hook store.NotifyFunc
	if *sendEmail {
		q, err := cliQueue(cfg, st)
		if err != nil {
			return err
		}
		hook = func(r store.IssueResult) (*store.NewEmail, error) {
			return web.CertificateReadyEmail(q, platformOf(cfg), sc.Client(), r.Email, r.FullName, r.CourseTitle,
				r.CertificateID, web.ClaimURL(cfg.BaseURL, r.ClaimToken))
		}
	}
	results, err := st.IssueAndNotify(ctx, sc, reqs, hook)
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
		switch {
		case r.Existing:
		case *sendEmail: // emailed to the student; don't spread the secret link further
			status = "emailed"
			issued++
		default:
			status, claim = "issued", web.ClaimURL(cfg.BaseURL, r.ClaimToken)
			issued++
		}
		cw.Write([]string{r.Email, r.FullName, r.CertificateID, cfg.BaseURL + "/c/" + r.CertificateID, claim, status})
	}
	cw.Flush()
	auditCLI(ctx, st, sc, "certificates.issue", "", "", map[string]any{"rows": len(results), "issued": issued, "emailed": *sendEmail})
	fmt.Fprintf(os.Stderr, "%d issued, %d already existed\n", issued, len(results)-issued)
	if *sendEmail && issued > 0 {
		fmt.Fprintf(os.Stderr, "%d emails queued; the running server delivers them (check with `certsforever email status`)\n", issued)
	}
	return cw.Error()
}

func revoke(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	client := fs.String("client", os.Getenv("CERTS_CLIENT"), "client slug (default: from the ID prefix)")
	reason := fs.String("reason", "", "reason (stored, not shown publicly)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever revoke [-client slug] [-reason text] CERT_ID")
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	sc, err := scopeForCert(ctx, st, *client, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := st.Revoke(ctx, sc, fs.Arg(0), *reason); err != nil {
		return err
	}
	auditCLI(ctx, st, sc, "certificate.revoke", "certificate", fs.Arg(0), map[string]any{"reason": *reason})
	fmt.Println("revoked", fs.Arg(0))
	return nil
}

func claimLink(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("claim-link", flag.ExitOnError)
	client := fs.String("client", os.Getenv("CERTS_CLIENT"), "client slug (default: from the ID prefix)")
	sendEmail := fs.Bool("email", false, "email the new link to the student instead of printing it")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: certsforever claim-link [-client slug] [-email] CERT_ID")
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	sc, err := scopeForCert(ctx, st, *client, fs.Arg(0))
	if err != nil {
		return err
	}
	if *sendEmail {
		q, err := cliQueue(cfg, st)
		if err != nil {
			return err
		}
		var to string
		_, err = st.NewClaimLinkAndNotify(ctx, sc, fs.Arg(0), func(c *store.Certificate, tok string) (*store.NewEmail, error) {
			to = c.Email
			return web.CertificateReadyEmail(q, platformOf(cfg), sc.Client(), c.Email, c.RecipientName, c.CourseTitle,
				c.ID, web.ClaimURL(cfg.BaseURL, tok))
		})
		if err != nil {
			return err
		}
		auditCLI(ctx, st, sc, "certificate.claim_link", "certificate", fs.Arg(0), map[string]any{"emailed": true})
		fmt.Printf("new claim link queued for %s; earlier links no longer work\n", to)
		return nil
	}
	token, err := st.NewClaimLink(ctx, sc, fs.Arg(0))
	if err != nil {
		return err
	}
	auditCLI(ctx, st, sc, "certificate.claim_link", "certificate", fs.Arg(0), nil)
	fmt.Println(web.ClaimURL(cfg.BaseURL, token))
	return nil
}

func demo(ctx context.Context, cfg config.Config) error {
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	sc, err := st.Scope(ctx, "zcw")
	if errors.Is(err, store.ErrNotFound) {
		name, prefix := "Zip Code Wilmington", "ZCW"
		site := "https://zipcodewilmington.com"
		blurb := "Zip Code Wilmington is a nonprofit, intensive coding bootcamp in Wilmington, Delaware, " +
			"preparing people from all backgrounds for careers in software."
		if _, err = st.CreateClient(ctx, store.ClientInput{Slug: "zcw", Name: &name, IDPrefix: &prefix,
			SiteURL: &site, Blurb: &blurb}); err != nil {
			return err
		}
		sc, err = st.Scope(ctx, "zcw")
	}
	if err != nil {
		return err
	}
	if _, err := st.CreateCourse(ctx, sc, store.Course{
		Slug:   "java-fullstack",
		Title:  "Java Full-Stack Developer",
		Skills: []string{"Java", "Spring Boot", "SQL", "REST APIs", "Git", "Agile"},
		Hours:  480,
	}); err != nil {
		return err
	}
	res, err := st.Issue(ctx, sc, []store.IssueRequest{{
		Email: "demo.student@example.com", FullName: "Grace Hopper",
		CourseSlug: "java-fullstack", Cohort: "Java 13",
		CompletedOn: time.Now().UTC().Truncate(24 * time.Hour),
	}})
	if err != nil {
		return err
	}
	r := res[0]
	token := r.ClaimToken
	if token == "" {
		if token, err = st.NewClaimLink(ctx, sc, r.CertificateID); err != nil {
			return err
		}
	}
	if err := st.SetVisibilityByClaimToken(ctx, token, "public"); err != nil {
		return err
	}
	fmt.Println("public page: ", cfg.BaseURL+"/c/"+r.CertificateID)
	fmt.Println("share image: ", cfg.BaseURL+"/c/"+r.CertificateID+"/og.png")
	fmt.Println("claim page:  ", web.ClaimURL(cfg.BaseURL, token))
	return nil
}

// resolveScope picks the client for a command: the -client flag (or
// CERTS_CLIENT), or the only client if there is exactly one.
func resolveScope(ctx context.Context, st *store.Store, slug string) (store.Scope, error) {
	if slug != "" {
		sc, err := st.Scope(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return sc, fmt.Errorf("no client %q (see `certsforever client list`)", slug)
		}
		return sc, err
	}
	clients, err := st.ListClients(ctx)
	if err != nil {
		return store.Scope{}, err
	}
	switch len(clients) {
	case 0:
		return store.Scope{}, errors.New("no clients yet; create one with `certsforever client add`")
	case 1:
		return st.Scope(ctx, clients[0].Slug)
	}
	slugs := make([]string, len(clients))
	for i, c := range clients {
		slugs[i] = c.Slug
	}
	return store.Scope{}, fmt.Errorf("several clients exist (%s); pass -client or set CERTS_CLIENT",
		strings.Join(slugs, ", "))
}

// scopeForCert uses -client if given, otherwise the client that owns the
// certificate ID's prefix.
func scopeForCert(ctx context.Context, st *store.Store, slug, certID string) (store.Scope, error) {
	if slug != "" {
		return resolveScope(ctx, st, slug)
	}
	sc, err := st.ScopeForCertificate(ctx, certID)
	if errors.Is(err, store.ErrNotFound) {
		return sc, fmt.Errorf("no client uses the prefix of %q", certID)
	}
	return sc, err
}

func clientCmd(ctx context.Context, cfg config.Config, args []string) error {
	usage := errors.New(`usage:
  certsforever client list
  certsforever client add    -slug zcw -name "Zip Code Wilmington" -prefix ZCW [-site URL] [-blurb TEXT] [-linkedin-org ID]
                             [-reply-to ADDRESS] [-reminders on|off]
  certsforever client update -slug zcw [-name ...] [-prefix ...] [-site ...] [-blurb ...] [-linkedin-org ...]
                             [-reply-to ...] [-reminders on|off]
  certsforever client suspend  -slug zcw
  certsforever client activate -slug zcw`)
	if len(args) == 0 {
		return usage
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("client "+sub, flag.ExitOnError)
	slug := fs.String("slug", "", "client slug (2–40 lowercase letters, digits, dashes)")
	name := fs.String("name", "", "display name")
	prefix := fs.String("prefix", "", "certificate ID prefix, 2–4 letters (frozen after the first certificate)")
	site := fs.String("site", "", "program website (\"Learn about the program\" link)")
	blurb := fs.String("blurb", "", "one or two sentences shown on certificate pages")
	linkedin := fs.String("linkedin-org", "", "numeric LinkedIn company page ID")
	replyTo := fs.String("reply-to", "", "Reply-To address on emails to this client's students")
	reminders := fs.String("reminders", "", "on|off: one publish reminder after 7 days (default on)")
	fs.Parse(args)

	// Only flags actually given are changed on update.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	opt := func(flagName string, v *string) *string {
		if set[flagName] {
			return v
		}
		return nil
	}
	in := store.ClientInput{Slug: *slug, Name: opt("name", name), IDPrefix: opt("prefix", prefix),
		SiteURL: opt("site", site), Blurb: opt("blurb", blurb), LinkedInOrgID: opt("linkedin-org", linkedin),
		ReplyTo: opt("reply-to", replyTo)}
	if set["reminders"] {
		on := strings.EqualFold(*reminders, "on") || *reminders == "true"
		if !on && !strings.EqualFold(*reminders, "off") && *reminders != "false" {
			return errors.New("-reminders must be on or off")
		}
		in.SendReminders = &on
	}

	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()

	show := func(c *store.Client) {
		fmt.Printf("%-12s %-5s %-9s %s  %s\n", c.Slug, c.IDPrefix, c.Status, c.Name, c.SiteURL)
	}
	switch sub {
	case "list":
		list, err := st.ListClients(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no clients yet")
		}
		for i := range list {
			show(&list[i])
		}
		return nil
	case "add":
		c, err := st.CreateClient(ctx, in)
		if err != nil {
			return err
		}
		if sc, err := st.Scope(ctx, c.Slug); err == nil {
			auditCLI(ctx, st, sc, "client.create", "client", c.Slug, map[string]any{"name": c.Name, "id_prefix": c.IDPrefix})
		}
		show(c)
		return nil
	case "update":
		if *slug == "" {
			return usage
		}
		c, err := st.UpdateClient(ctx, in)
		if err != nil {
			return err
		}
		if sc, err := st.Scope(ctx, c.Slug); err == nil {
			auditCLI(ctx, st, sc, "client.update", "client", c.Slug, nil)
		}
		show(c)
		return nil
	case "suspend", "activate":
		if *slug == "" {
			return usage
		}
		status := map[string]string{"suspend": "suspended", "activate": "active"}[sub]
		if err := st.SetClientStatus(ctx, *slug, status); err != nil {
			return err
		}
		if sc, err := st.Scope(ctx, *slug); err == nil {
			auditCLI(ctx, st, sc, "client.status", "client", *slug, map[string]any{"status": status})
		}
		fmt.Printf("client %s is now %s\n", *slug, status)
		return nil
	}
	return usage
}

// newMailer picks how email is delivered: SMTP when configured; otherwise
// the log in development (emails, including sign-in links, appear in the
// server output) and nothing in production (nil: email is off; use
// `certsforever login-link`).
func newMailer(cfg config.Config, log *slog.Logger) (mail.Sender, string, error) {
	switch {
	case cfg.SMTPURL != "":
		m, err := mail.NewSMTP(cfg.SMTPURL, cfg.MailFrom)
		return m, "SMTP, from " + cfg.MailFrom, err
	case cfg.Production():
		log.Warn("CERTS_SMTP_URL not set: email is off; sign-in and invitation links must be printed with " +
			"`certsforever login-link EMAIL`, and students aren't emailed")
		return nil, "off: CERTS_SMTP_URL not set", nil
	default:
		log.Warn("development mode without CERTS_SMTP_URL: emails (including sign-in links) are written to this log")
		return mail.Log{Logger: log}, "development: written to the server log", nil
	}
}

// cliQueue is the outbox for CLI commands that queue email. The running
// server's worker delivers it (within its poll interval).
func cliQueue(cfg config.Config, st *store.Store) (*outbox.Queue, error) {
	if cfg.SMTPURL == "" && cfg.Production() {
		return nil, errors.New("email isn't configured (CERTS_SMTP_URL); leave off -email and send the links yourself")
	}
	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	return outbox.NewQueue(st, box), nil
}

func platformOf(cfg config.Config) emails.Platform {
	return emails.Platform{Name: cfg.PlatformName, BaseURL: cfg.BaseURL}
}

// auditCLI records an action taken from the command line. The zero Scope
// means a platform-level action.
func auditCLI(ctx context.Context, st *store.Store, sc store.Scope, action, targetType, targetID string, details map[string]any) {
	actor := "cli"
	if u := os.Getenv("USER"); u != "" {
		actor = "cli:" + u
	}
	if err := st.Audit(ctx, sc, store.AuditEntry{Actor: actor, Action: action, TargetType: targetType,
		TargetID: targetID, Details: details}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: audit log write failed:", err)
	}
}

func printLoginLink(ctx context.Context, cfg config.Config, st *store.Store, u *store.User) error {
	token, err := st.CreateLoginToken(ctx, u.ID, "login", store.BootstrapLinkTTL)
	if err != nil {
		return err
	}
	fmt.Printf("\nOne-time sign-in link for %s (expires in %d minutes):\n\n  %s/login/%s\n\n",
		u.Email, int(store.BootstrapLinkTTL.Minutes()), cfg.BaseURL, token)
	fmt.Println("Treat it like a password: anyone with it can sign in as this user.")
	return nil
}

func superadminCmd(ctx context.Context, cfg config.Config, args []string) error {
	usage := errors.New(`usage:
  certsforever superadmin add EMAIL [-name NAME]   grant platform admin and print a sign-in link
  certsforever superadmin list
  certsforever superadmin remove EMAIL`)
	if len(args) == 0 {
		return usage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("superadmin "+sub, flag.ExitOnError)
	name := fs.String("name", "", "display name")
	// Accept the email before or after flags.
	var email string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		email, rest = rest[0], rest[1:]
	}
	fs.Parse(rest)
	if email == "" && fs.NArg() > 0 {
		email = fs.Arg(0)
	}

	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "list":
		list, err := st.ListSuperAdmins(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no platform administrators yet; add one with `certsforever superadmin add EMAIL`")
		}
		for _, u := range list {
			twoStep := "2-step off"
			if u.TOTPEnabled {
				twoStep = "2-step on"
			}
			fmt.Printf("%-32s %-10s %s\n", u.Email, twoStep, u.Name)
		}
		return nil
	case "add":
		if email == "" {
			return usage
		}
		u, _, err := st.EnsureUser(ctx, email, *name)
		if err != nil {
			return err
		}
		if err := st.SetSuperAdmin(ctx, u.ID, true); err != nil {
			return err
		}
		auditCLI(ctx, st, store.Scope{}, "super.grant", "user", u.Email, nil)
		fmt.Printf("%s is a platform administrator. They'll set up two-step verification at first sign-in.\n", u.Email)
		return printLoginLink(ctx, cfg, st, u)
	case "remove":
		if email == "" {
			return usage
		}
		u, err := st.GetUserByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("no user %s", email)
		}
		if err := st.SetSuperAdmin(ctx, u.ID, false); err != nil {
			return err
		}
		auditCLI(ctx, st, store.Scope{}, "super.revoke", "user", u.Email, nil)
		fmt.Printf("%s is no longer a platform administrator\n", u.Email)
		return nil
	}
	return usage
}

func loginLinkCmd(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: certsforever login-link EMAIL")
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.GetUserByEmail(ctx, args[0])
	if err != nil || u.Disabled {
		return fmt.Errorf("no active user %s (add them with `superadmin add` or invite them from a client's page)", args[0])
	}
	auditCLI(ctx, st, store.Scope{}, "auth.login_link_printed", "user", u.Email, nil)
	return printLoginLink(ctx, cfg, st, u)
}

func userCmd(ctx context.Context, cfg config.Config, args []string) error {
	usage := errors.New(`usage:
  certsforever user reset-2fa EMAIL   remove two-step verification (lost phone); signs them out everywhere
  certsforever user disable EMAIL     block sign-in and end their sessions
  certsforever user enable EMAIL`)
	if len(args) != 2 {
		return usage
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.GetUserByEmail(ctx, args[1])
	if err != nil {
		return fmt.Errorf("no user %s", args[1])
	}
	switch args[0] {
	case "reset-2fa":
		err = st.ResetTOTP(ctx, u.ID)
	case "disable":
		err = st.SetUserDisabled(ctx, u.ID, true)
	case "enable":
		err = st.SetUserDisabled(ctx, u.ID, false)
	default:
		return usage
	}
	if err != nil {
		return err
	}
	auditCLI(ctx, st, store.Scope{}, "user."+args[0], "user", u.Email, nil)
	fmt.Printf("%s: %s done\n", u.Email, args[0])
	return nil
}

func emailCmd(ctx context.Context, cfg config.Config, args []string) error {
	usage := errors.New(`usage:
  certsforever email status               queue counts, failed messages, suppressed addresses
  certsforever email test ADDRESS         send a test email now and report the result (checks SMTP)
  certsforever email retry ID             re-queue a failed message
  certsforever email suppress ADDRESS [REASON]
  certsforever email unsuppress ADDRESS`)
	if len(args) == 0 {
		return usage
	}
	st, err := openStore(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "status":
		counts, err := st.OutboxCounts(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("queued %d · sending %d · sent %d · failed %d · suppressed %d\n",
			counts["queued"], counts["sending"], counts["sent"], counts["failed"], counts["suppressed"])
		failed, _ := st.ListEmails(ctx, "failed", 20)
		if len(failed) > 0 {
			fmt.Println("\nfailed:")
			for _, it := range failed {
				retry := "retryable"
				switch {
				case it.Suppressed:
					retry = "address suppressed"
				case !it.HasBody:
					retry = "expired"
				}
				fmt.Printf("  #%-6d %-30s %-18s %d attempts  %s  (%s)\n", it.ID, it.To, it.Template, it.Attempts, it.LastError, retry)
			}
		}
		sup, _ := st.ListSuppressions(ctx, 20)
		if len(sup) > 0 {
			fmt.Println("\nsuppressed:")
			for _, sp := range sup {
				fmt.Printf("  %-32s %-8s %s\n", sp.Email, sp.Source, sp.Reason)
			}
		}
		return nil
	case "test":
		if len(args) != 2 {
			return usage
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		sender, mode, err := newMailer(cfg, log)
		if err != nil {
			return err
		}
		if sender == nil {
			return errors.New("email is off: set CERTS_SMTP_URL and CERTS_MAIL_FROM")
		}
		m, err := emails.Test(platformOf(cfg), args[1])
		if err != nil {
			return err
		}
		sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := sender.Send(sendCtx, m); err != nil {
			return fmt.Errorf("test email failed (%s): %w", mode, err)
		}
		fmt.Printf("test email sent to %s via %s\n", args[1], mode)
		return nil
	case "retry":
		if len(args) != 2 {
			return usage
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[1], "#"), 10, 64)
		if err != nil {
			return usage
		}
		if err := st.RetryFailedEmail(ctx, id); err != nil {
			return err
		}
		auditCLI(ctx, st, store.Scope{}, "email.retry", "email", args[1], nil)
		fmt.Println("re-queued; the running server delivers it")
		return nil
	case "suppress":
		if len(args) < 2 {
			return usage
		}
		reason := strings.Join(args[2:], " ")
		if err := st.Suppress(ctx, args[1], reason, "manual"); err != nil {
			return err
		}
		auditCLI(ctx, st, store.Scope{}, "email.suppressed", "email", args[1], map[string]any{"source": "manual"})
		fmt.Println("suppressed", args[1])
		return nil
	case "unsuppress":
		if len(args) != 2 {
			return usage
		}
		if err := st.Unsuppress(ctx, args[1]); err != nil {
			return fmt.Errorf("%s isn't suppressed", args[1])
		}
		auditCLI(ctx, st, store.Scope{}, "email.unsuppressed", "email", args[1], nil)
		fmt.Println(args[1], "can receive email again")
		return nil
	}
	return usage
}

// Command certsforever runs the certificate server and its admin CLI.
//
//	certsforever serve                         run the HTTP server
//	certsforever course -slug java -title "Java Full-Stack Developer" -skills "Java,Spring,SQL"
//	certsforever import cohort.csv [-out links.csv]
//	certsforever revoke -reason "issued in error" ZCW-XXXXXXXXXX
//	certsforever claim-link ZCW-XXXXXXXXXX
//	certsforever demo                          seed a sample public certificate
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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	cfg := config.FromEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx, cfg)
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
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
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
  demo         seed a sample public certificate for local testing`)
}

func openStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, cfg.DBPath)
}

func serve(ctx context.Context, cfg config.Config) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st, err := openStore(ctx, cfg)
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
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "db", cfg.DBPath)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func course(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("course", flag.ExitOnError)
	slug := fs.String("slug", "", "URL-safe identifier, e.g. java")
	title := fs.String("title", "", "title printed on the certificate")
	desc := fs.String("description", "", "short description")
	skills := fs.String("skills", "", "comma-separated skills")
	hours := fs.Int("hours", 0, "instructional hours")
	fs.Parse(args)

	st, err := openStore(ctx, cfg)
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
	st, err := openStore(ctx, cfg)
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
		of, err := os.Create(*out)
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
	st, err := openStore(ctx, cfg)
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
	st, err := openStore(ctx, cfg)
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
	st, err := openStore(ctx, cfg)
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

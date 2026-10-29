package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"certsforever/internal/buildinfo"
	"certsforever/internal/importer"
	"certsforever/internal/store"
)

// The MCP endpoint lets an AI assistant (Claude, for example) work with one
// client's certificates over the Model Context Protocol. It authenticates
// with the client's API token, exactly like /api/v1, so it sees only that
// client, is rate limited per token, and its writes are audited as
// "api-token:<name>" with "via": "mcp". It is stateless: every request builds
// a fresh MCP server bound to that request's scope, so nothing about one
// request (or one token) carries over to the next.
//
// Writes are limited to low-risk console actions: correct a name, resend a
// claim link, remind a cohort. Issuing and revoking stay in the console and
// /api/v1.

// maxMCPBody caps a request: room for a 5 MB issue-preview CSV plus JSON framing.
const maxMCPBody = maxCSV + 1<<20

// mcpScopeKey carries the token's scope from apiToken to the MCP server factory.
type mcpScopeKey struct{}

// mcpHandler returns the /mcp handler: apiToken authentication, then the
// SDK's streamable HTTP transport in stateless JSON mode.
func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		sc, ok := r.Context().Value(mcpScopeKey{}).(store.Scope)
		if !ok {
			return nil // unreachable behind apiToken; the SDK answers 400
		}
		return s.newMCPServer(r, sc)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return s.apiToken(func(w http.ResponseWriter, r *http.Request, sc store.Scope) {
		r.Body = http.MaxBytesReader(w, r.Body, maxMCPBody)
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpScopeKey{}, sc)))
	})
}

// Tool inputs. The json and jsonschema tags become each tool's input schema.
type (
	mcpNoInput    struct{}
	mcpCertFilter struct {
		Course  string `json:"course,omitempty" jsonschema:"course slug, from list_courses"`
		Cohort  string `json:"cohort,omitempty" jsonschema:"cohort name, from list_cohorts"`
		Query   string `json:"query,omitempty" jsonschema:"matches recipient name, email or certificate ID"`
		Status  string `json:"status,omitempty" jsonschema:"active or revoked"`
		Claimed string `json:"claimed,omitempty" jsonschema:"yes or no: whether the student has opened their claim link"`
		Limit   int    `json:"limit,omitempty" jsonschema:"page size, 1 to 100 (default 25)"`
		Offset  int    `json:"offset,omitempty" jsonschema:"rows to skip, for paging"`
	}
	mcpCohortInput struct {
		Course string `json:"course" jsonschema:"course slug"`
		Cohort string `json:"cohort" jsonschema:"cohort name"`
	}
	mcpCertID struct {
		ID string `json:"id" jsonschema:"certificate ID, such as ZCW-7K3M9QF2XA"`
	}
	mcpCSVInput struct {
		CSV string `json:"csv" jsonschema:"CSV with the header email,full_name,course,cohort,completed_on (dates YYYY-MM-DD)"`
	}
	mcpNameInput struct {
		ID   string `json:"id" jsonschema:"certificate ID"`
		Name string `json:"name" jsonschema:"the corrected full name"`
	}
)

// Tool outputs. Structured output must be a JSON object, so lists are wrapped.
type (
	mcpClient struct {
		Slug   string `json:"slug"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	mcpOverviewOut struct {
		Client   mcpClient           `json:"client"`
		Overview store.Overview      `json:"overview"`
		Courses  []store.CourseStats `json:"courses"`
	}
	mcpCoursesOut struct {
		Courses []store.Course `json:"courses"`
	}
	mcpCohortsOut struct {
		Cohorts []store.CohortSummary `json:"cohorts"`
	}
	mcpCertsOut struct {
		Certificates []store.Certificate `json:"certificates"`
		Total        int                 `json:"total"`
	}
	mcpRosterOut struct {
		Certificates []store.Certificate `json:"certificates"`
		EmailStatus  map[string]string   `json:"email_status" jsonschema:"latest email status per certificate ID: queued, sending, sent, failed or suppressed"`
	}
	mcpCertOut struct {
		Certificate store.Certificate  `json:"certificate"`
		URL         string             `json:"url"`
		Events      map[string]int     `json:"events" jsonschema:"counts: view, og_image, linkedin_add, linkedin_share, learn_more"`
		Emails      []store.OutboxItem `json:"emails"`
		History     []store.AuditEntry `json:"history"`
	}
	mcpNameOut struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	mcpResendOut struct {
		ID      string `json:"id"`
		Emailed bool   `json:"emailed"`
	}
	mcpRemindOut struct {
		Sent    int `json:"sent"`
		Skipped int `json:"skipped"`
	}
)

// newMCPServer builds the MCP server for one request. r is used for the
// audit log (actor and IP); sc is the token's client.
func (s *Server) newMCPServer(r *http.Request, sc store.Scope) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "laurelposts", Version: buildinfo.Get().Version}, nil)
	s.addMCPReadTools(srv, sc)
	s.addMCPWriteTools(srv, r, sc)
	return srv
}

// mcpRead is the annotation shared by the read tools.
var mcpRead = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}

// addMCPReadTools registers the tools that change nothing.
func (s *Server) addMCPReadTools(srv *mcp.Server, sc store.Scope) {
	mcp.AddTool(srv, &mcp.Tool{Name: "overview", Annotations: mcpRead,
		Description: "Headline numbers for this client: courses, certificates issued, claimed, public and revoked, views and learn-more clicks in the last 30 days, failed emails, and per-course share and LinkedIn counts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpOverviewOut, error) {
			cl := sc.Client()
			out := mcpOverviewOut{Client: mcpClient{Slug: cl.Slug, Name: cl.Name, Status: cl.Status}}
			o, err := s.store.GetOverview(ctx, sc)
			if err != nil {
				return nil, out, s.mcpError(err)
			}
			out.Overview = *o
			out.Courses, err = s.store.Stats(ctx, sc)
			return nil, out, s.mcpError(err)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_courses", Annotations: mcpRead, Description: "This client's courses, with slugs, titles and skills."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpCoursesOut, error) {
			c, err := s.store.ListCourses(ctx, sc)
			return nil, mcpCoursesOut{c}, s.mcpError(err)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_cohorts", Annotations: mcpRead,
		Description: "This client's cohorts, newest first, with how many certificates are claimed, public, revoked and still unclaimed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpCohortsOut, error) {
			c, err := s.store.ListCohorts(ctx, sc)
			return nil, mcpCohortsOut{c}, s.mcpError(err)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "search_certificates", Annotations: mcpRead,
		Description: "Search this client's certificates by course, cohort, name/email/ID, status and claimed. Returns one page and the total matching."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCertFilter) (*mcp.CallToolResult, mcpCertsOut, error) {
			f := store.CertFilter{Course: in.Course, Cohort: in.Cohort, Query: in.Query, Status: in.Status, Claimed: in.Claimed,
				Limit: in.Limit, Offset: max(in.Offset, 0)}
			if f.Limit <= 0 {
				f.Limit = 25
			}
			f.Limit = min(f.Limit, 100)
			c, n, err := s.store.SearchCertificates(ctx, sc, f)
			return nil, mcpCertsOut{c, n}, s.mcpError(err)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "cohort_roster", Annotations: mcpRead,
		Description: "Every certificate in one cohort, with the status of the latest email to each student."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCohortInput) (*mcp.CallToolResult, mcpRosterOut, error) {
			out := mcpRosterOut{}
			var err error
			if out.Certificates, _, err = s.store.SearchCertificates(ctx, sc, store.CertFilter{Course: in.Course, Cohort: in.Cohort}); err != nil {
				return nil, out, s.mcpError(err)
			}
			if len(out.Certificates) == 0 {
				return nil, out, errors.New("there's no cohort by that name here")
			}
			ids := make([]string, len(out.Certificates))
			for i, c := range out.Certificates {
				ids[i] = c.ID
			}
			out.EmailStatus, err = s.store.LatestEmailStatus(ctx, sc, ids)
			return nil, out, s.mcpError(err)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "get_certificate", Annotations: mcpRead,
		Description: "One certificate in full: its public URL, view and LinkedIn counts, the emails sent about it and its audit history."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCertID) (*mcp.CallToolResult, mcpCertOut, error) {
			out, err := s.mcpCertificate(ctx, sc, in.ID)
			return nil, out, err
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "preview_issue", Annotations: mcpRead,
		Description: "Dry run of issuing certificates from a CSV: what each row would do (new, existing, duplicate or error) and why. Issues nothing; issuing is done in the console."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCSVInput) (*mcp.CallToolResult, *store.Preview, error) {
			reqs, err := importer.Parse(strings.NewReader(in.CSV))
			if err != nil {
				return nil, nil, errors.New("the CSV has problems: " + err.Error())
			}
			p, err := s.store.PreviewIssue(ctx, sc, reqs)
			return nil, p, s.mcpError(err)
		})
}

// mcpCertificate gathers get_certificate's result for certificate id.
// Returns a tool error when the ID isn't this client's.
func (s *Server) mcpCertificate(ctx context.Context, sc store.Scope, id string) (mcpCertOut, error) {
	out := mcpCertOut{}
	c, err := s.store.GetClientCertificate(ctx, sc, id)
	if err != nil {
		return out, s.mcpError(err)
	}
	out.Certificate, out.URL, out.Events = *c, s.certURL(c), map[string]int{}
	ev, err := s.store.CertificateEvents(ctx, sc, c.ID)
	if err != nil {
		return out, s.mcpError(err)
	}
	for k, n := range ev {
		out.Events[string(k)] = n
	}
	if out.Emails, err = s.store.CertificateEmails(ctx, sc, c.ID); err != nil {
		return out, s.mcpError(err)
	}
	out.History, err = s.store.ListAuditForTarget(ctx, sc, "certificate", c.ID, 50)
	return out, s.mcpError(err)
}

// addMCPWriteTools registers the low-risk write tools. Each is audited
// against r, the request that carried it.
func (s *Server) addMCPWriteTools(srv *mcp.Server, r *http.Request, sc store.Scope) {
	no, yes := false, true
	mcp.AddTool(srv, &mcp.Tool{Name: "correct_name", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no},
		Description: "Correct the recipient's name on a certificate, for a typo or a preferred name. The certificate's ID and link don't change."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpNameInput) (*mcp.CallToolResult, mcpNameOut, error) {
			old, err := s.store.CorrectName(ctx, sc, in.ID, in.Name)
			if err != nil {
				return nil, mcpNameOut{}, s.mcpError(err)
			}
			out := mcpNameOut{From: old, To: in.Name}
			if c, err := s.store.GetClientCertificate(ctx, sc, in.ID); err == nil {
				out.To = c.RecipientName
			}
			s.audit(r, nil, sc, "certificate.name_correct", "certificate", in.ID, map[string]any{"from": out.From, "to": out.To, "via": "mcp"})
			return nil, out, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "resend_claim_link", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: &yes},
		Description: "Email the student a new claim link for their certificate. The old link stops working. Refused for revoked certificates and bounced addresses."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCertID) (*mcp.CallToolResult, mcpResendOut, error) {
			if !s.mailConfigured() {
				return nil, mcpResendOut{}, errors.New("Email isn't configured on this server.")
			}
			if err := s.resendClaimLink(ctx, sc, in.ID); err != nil {
				return nil, mcpResendOut{}, s.mcpError(err)
			}
			s.audit(r, nil, sc, "certificate.claim_link", "certificate", in.ID, map[string]any{"emailed": true, "via": "mcp"})
			return nil, mcpResendOut{ID: in.ID, Emailed: true}, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "remind_cohort", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &yes},
		Description: "Email a reminder to everyone in a cohort who hasn't opened their claim link. Skips revoked certificates, bounced addresses and anyone emailed in the last day."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCohortInput) (*mcp.CallToolResult, mcpRemindOut, error) {
			if !s.mailConfigured() {
				return nil, mcpRemindOut{}, errors.New("Email isn't configured on this server.")
			}
			sent, skipped, err := s.remindCohort(ctx, sc, in.Course, in.Cohort)
			if err != nil {
				return nil, mcpRemindOut{}, s.mcpError(err)
			}
			s.audit(r, nil, sc, "cohort.remind", "cohort", in.Course+"/"+in.Cohort, map[string]any{"sent": sent, "skipped": skipped, "via": "mcp"})
			return nil, mcpRemindOut{Sent: sent, Skipped: skipped}, nil
		})
}

// mcpError turns a store error into a tool error the assistant can act on.
// Validation and conflict messages pass through; unknown IDs read as "not
// here"; anything else is logged and reported only as an internal error.
// Returns nil for a nil err.
func (s *Server) mcpError(err error) error {
	if err == nil {
		return nil
	}
	if msg, ok := userError(err); ok {
		return errors.New(msg)
	}
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("there's no certificate with that ID here")
	}
	s.log.Error("mcp", "err", err)
	return errors.New("internal error")
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/approvals"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
	"github.com/abhishekjha/close-copilot/internal/telemetry/llmtrace"
	"github.com/abhishekjha/close-copilot/internal/web"
)

// Fixed errors for a missing key; they never carry a value.
var (
	errSessionKey = errors.New("agent: serve needs " + config.EnvAppSessionKey + " (at least 32 bytes; see .env.example)")
	errScopeKey   = errors.New("agent: serve needs " + config.EnvMCPScopeKey + " (the key the MCP servers verify run scopes with)")
)

// checkServeKeys refuses to start without the keys serve needs.
func checkServeKeys(cfg config.Config) error {
	if cfg.AppSessionKey.IsZero() {
		return errSessionKey
	}
	if cfg.MCPScopeKey.IsZero() {
		return errScopeKey
	}
	return nil
}

// serveBackends are what the web server's run, ask and approvals routes
// call. Ask may be nil (--no-explain): /api/ask then answers 501.
// Approvals may be nil (no admin token): the proposal routes and
// /approvals then answer 501.
type serveBackends struct {
	runs      *web.RunsBackend
	ask       web.AskBackend
	approvals web.ApprovalsBackend
}

// newServeServer builds the web server on APP_ADDR and APP_SESSION_KEY.
func newServeServer(cfg config.Config, users web.Users, be serveBackends, log *slog.Logger) (*web.Server, error) {
	if err := checkServeKeys(cfg); err != nil {
		return nil, err
	}
	opts := web.OptionsFromConfig(cfg)
	opts.Runs = be.runs
	opts.Ask = be.ask
	opts.Approvals = be.approvals
	return web.NewServer(opts, users, log)
}

// approvalsStore is what the approvals service needs of the store;
// *store.Store satisfies it.
type approvalsStore interface {
	approvals.ProposalStore
	approvals.FindingLookup
}

var _ approvalsStore = (*store.Store)(nil)

// newApprovals builds the maker-checker backend over st. The whole config
// goes to approvals.NewPoster, which alone reads the admin token. When the
// poster can't be built (no admin token, no books URL) the server still
// runs: the reason is logged once at Warn and the approvals routes answer
// 501.
func newApprovals(cfg config.Config, st approvalsStore, log *slog.Logger) web.ApprovalsBackend {
	poster, err := approvals.NewPoster(cfg, log)
	if err != nil {
		log.Warn("agent: approvals are disabled; the proposal routes answer 501", "reason", err.Error())
		return nil
	}
	return approvalsAdapter{svc: approvals.New(approvals.Deps{Store: st, Poster: poster, Findings: st, Logger: log})}
}

// approvalsAdapter adapts approvals.Service to web.ApprovalsBackend, so
// internal/web never imports internal/approvals.
type approvalsAdapter struct {
	svc *approvals.Service
}

func (a approvalsAdapter) Propose(ctx context.Context, actor string, findingID uuid.UUID) (store.JournalProposal, error) {
	p, err := a.svc.Propose(ctx, actor, findingID)
	return p, adaptApprovalErr(err)
}

func (a approvalsAdapter) Approve(ctx context.Context, actor string, id uuid.UUID) (web.ApproveResult, error) {
	r, err := a.svc.Approve(ctx, actor, id)
	if err != nil {
		return web.ApproveResult{}, adaptApprovalErr(err)
	}
	return web.ApproveResult{ProposalID: r.ProposalID, Status: r.Status, ERPName: r.ERPName, AlreadyPosted: r.AlreadyPosted}, nil
}

func (a approvalsAdapter) Reject(ctx context.Context, actor string, id uuid.UUID, reason string) error {
	return adaptApprovalErr(a.svc.Reject(ctx, actor, id, reason))
}

func (a approvalsAdapter) Pending(ctx context.Context) ([]store.JournalProposal, error) {
	return a.svc.Pending(ctx)
}

func (a approvalsAdapter) Get(ctx context.Context, id uuid.UUID) (store.JournalProposal, error) {
	p, err := a.svc.Get(ctx, id)
	return p, adaptApprovalErr(err)
}

// approvalKinds maps the service's error kinds to the web's.
var approvalKinds = []struct{ from, to error }{
	{approvals.ErrNotFound, web.ErrApprovalNotFound},
	{approvals.ErrInvalidProposal, web.ErrApprovalInvalid},
	{approvals.ErrInvalidReason, web.ErrApprovalInvalid},
	{approvals.ErrSelfApproval, web.ErrApprovalForbidden},
	{approvals.ErrNotProposable, web.ErrApprovalConflict},
	{approvals.ErrProposalExists, web.ErrApprovalConflict},
	{approvals.ErrNotPending, web.ErrApprovalConflict},
	{approvals.ErrPostFailed, web.ErrApprovalPostFailed},
}

// adaptApprovalErr turns a service refusal into a *web.ApprovalError with
// the service's fixed message. Any other error passes through (a 500).
func adaptApprovalErr(err error) error {
	var e *approvals.Error
	if err == nil || !errors.As(err, &e) {
		return err
	}
	for _, k := range approvalKinds {
		if errors.Is(e, k.from) {
			return &web.ApprovalError{Kind: k.to, Message: e.Msg}
		}
	}
	return err
}

// askAdapter adapts agent.Asker to web.AskBackend, so internal/web imports
// neither internal/agent nor the ERPNext client.
type askAdapter struct {
	asker *agent.Asker
}

func (a askAdapter) Ask(ctx context.Context, in web.AskInput) (web.AskOutput, error) {
	// in.Company is the validated company field, never text from the question.
	res, err := a.asker.Ask(ctx, agent.AskRequest{Question: in.Question, Company: in.Company, Month: in.Month})
	switch {
	case errors.Is(err, agent.ErrQuestionTooLong):
		return web.AskOutput{}, fmt.Errorf("%w: %w", web.ErrQuestionTooLong, err)
	case errors.Is(err, agent.ErrAskReroutes):
		return web.AskOutput{}, fmt.Errorf("%w: %w", web.ErrTooManyAsks, err)
	case err != nil:
		// Any error means do not answer.
		return web.AskOutput{}, err
	}
	out := web.AskOutput{
		RunID: res.RunID, Action: res.Action, Refusal: res.Refusal, Answer: res.Answer,
		Citations: res.Citations, EvidenceIDs: res.EvidenceIDs, Verified: res.Verified,
		NeedsReview: res.NeedsReview, StepRefs: res.StepRefs,
	}
	for _, v := range res.Violations {
		out.Violations = append(out.Violations, web.AskViolation{Code: v.Code, Field: v.Field, ValuePaise: v.ValuePaise, Detail: v.Detail})
	}
	return out, nil
}

// serve runs the web server until ctx is cancelled.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger, cmd command) error {
	if err := checkServeKeys(cfg); err != nil {
		return err
	}
	key, err := explainKey(cfg, log, cmd)
	if err != nil {
		return err
	}
	users, err := web.LoadUsers(filepath.Join(cmd.configDir, "users.yaml"))
	if err != nil {
		return err
	}
	stopTrace, err := telemetry.Init(ctx, "agent", cfg, log)
	if err != nil {
		return err
	}
	defer stopTrace()

	d, err := openDeps(ctx, cfg, log, cmd)
	if err != nil {
		return err
	}
	defer d.close()

	var model llm.Provider
	var names []string
	if !cmd.noExplain {
		if model, err = newProvider(cfg, filepath.Join(cmd.configDir, "pricing.yaml")); err != nil {
			return err
		}
		// The company is known only per request: pseudonymise the names of
		// every profile's individuals.
		for _, p := range d.profiles {
			for _, n := range p.IndividualNames() {
				if !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
		}
	}
	wf, err := newWorkflow(cfg, log, cmd, d, model, key, names)
	if err != nil {
		return err
	}
	be := serveBackends{
		runs:      &web.RunsBackend{Store: d.st, Execute: executeRun(wf), Findings: d.st},
		approvals: newApprovals(cfg, d.st, log),
	}
	if !cmd.noExplain {
		asker, err := newAsker(cfg, log, cmd, d, model, key, names)
		if err != nil {
			return err
		}
		be.ask = askAdapter{asker: asker}
	}
	srv, err := newServeServer(cfg, users, be, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// executeRun runs a queued run inside this process: Workflow.ResumeClose,
// without its result.
func executeRun(wf *agent.Workflow) web.RunExecutor {
	return func(ctx context.Context, id uuid.UUID) error {
		_, err := wf.ResumeClose(ctx, id)
		return err
	}
}

// newAsker builds the question router and the Asker over the workflow's
// store and MCP registry. Both share one redactor closure over names.
func newAsker(cfg config.Config, log *slog.Logger, cmd command, d deps, model llm.Provider, key []byte, names []string) (*agent.Asker, error) {
	newRed, err := agent.AskRedactorFunc(key, names)
	if err != nil {
		return nil, err
	}
	rules, err := agent.LoadTriageRules(filepath.Join(cmd.configDir, "triage.yaml"))
	if err != nil {
		return nil, err
	}
	traced := llmtrace.Provider(model)
	tr, err := agent.NewTriage(rules, d.profiles, traced, d.st, newRed, d.st, cfg.LLMModelFast)
	if err != nil {
		return nil, err
	}
	tr.Log = log
	return agent.NewAsker(agent.AskerConfig{
		Triage: tr, Tools: d.reg, Retriever: &agent.MCPDocuments{Registry: d.reg},
		Store: d.st, Recorder: d.st, NewRedactor: newRed,
		Model: traced, ModelName: cfg.LLMModelStrong, Log: log,
	})
}

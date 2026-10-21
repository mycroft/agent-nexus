// Command agent-nexus listens for events and runs LLM agent jobs: v1 reviews
// pull requests on Gitea and GitHub with a llama-server backend.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/auth"
	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/config"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/metrics"
	"github.com/mycroft/agent-nexus/internal/models"
	"github.com/mycroft/agent-nexus/internal/remote"
	"github.com/mycroft/agent-nexus/internal/review"
	"github.com/mycroft/agent-nexus/internal/server"
	"github.com/mycroft/agent-nexus/internal/skill"
	"github.com/mycroft/agent-nexus/internal/web"
	"github.com/mycroft/agent-nexus/internal/zulip"
	"github.com/mycroft/agent-nexus/skills"
)

var version = "dev"

const usage = `usage: agent-nexus <command> [flags]

commands:
  serve     run the HTTP API and the job workers
  review    review one pull request now (see agent-nexus review -h)
  mcp       list the tools the configured MCP servers give the agent
  version   print the version

Configuration is read from the environment; see README.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "review":
		err = reviewOnce(os.Args[2:])
	case "mcp":
		err = checkMCP(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-nexus:", err)
		os.Exit(1)
	}
}

func newLogger(level string, json bool) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid LOG_LEVEL %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if json {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
}

// app holds what both commands build from the configuration.
type app struct {
	cfg      config.Config
	log      *slog.Logger
	forges   []forge.Forge
	reviewer *review.Reviewer
	// chatSkill guides answers on Zulip; it is loaded when Zulip is
	// configured.
	chatSkill skill.Skill
}

func newApp(ctx context.Context, cfg config.Config, log *slog.Logger) (*app, error) {
	a := &app{cfg: cfg, log: log}
	byName := map[string]forge.Forge{}
	if cfg.GiteaEnabled() {
		g := forge.NewGitea(cfg.GiteaURL, cfg.GiteaUser, cfg.GiteaToken)
		a.forges = append(a.forges, g)
		byName[g.Name()] = g
	}
	if cfg.GitHubEnabled() {
		g, err := forge.NewGitHub(cfg.GitHubAPIURL, cfg.GitHubServerURL, cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
		if err != nil {
			return nil, err
		}
		a.forges = append(a.forges, g)
		byName[g.Name()] = g
	}

	var skillFS fs.FS = skills.FS
	if cfg.SkillsDir != "" {
		skillFS = os.DirFS(cfg.SkillsDir)
	}
	prSkill, err := skill.Load(skillFS, "pr-review")
	if err != nil {
		return nil, err
	}
	discussionSkill, err := skill.Load(skillFS, "pr-discussion")
	if err != nil {
		return nil, err
	}
	if cfg.ZulipEnabled() {
		if a.chatSkill, err = skill.Load(skillFS, "chat"); err != nil {
			return nil, err
		}
	}
	modeRules, err := review.ParseModeRules(cfg.ReviewModes)
	if err != nil {
		return nil, fmt.Errorf("REVIEW_MODES: %w", err)
	}
	repos, err := gitrepo.NewCache(cfg.RepoCacheDir)
	if err != nil {
		return nil, err
	}
	remoteClient := remote.New(nil)
	chartFetcher, err := charts.New(cfg.ChartCacheDir, remoteClient)
	if err != nil {
		return nil, err
	}
	mcpClients, err := newMCPClients(ctx, cfg.MCPConfig, log)
	if err != nil {
		return nil, err
	}
	a.reviewer = &review.Reviewer{
		Forges:          byName,
		Repos:           repos,
		Skill:           prSkill,
		DiscussionSkill: discussionSkill,
		MaxTurns:        cfg.AgentMaxTurns,
		ModeRules:       modeRules,
		Charts:          chartFetcher,
		Remote:          remoteClient,
		FetchHosts:      cfg.FetchAllowedHosts,
		MCP:             mcpClients,
		Logger:          log,
	}
	return a, nil
}

// connect returns model m's context size, which it discovers from
// llama-server through client unless the configuration sets it.
func connect(ctx context.Context, m models.Model, client *llm.Client) (int, error) {
	if m.ContextTokens > 0 {
		return m.ContextTokens, nil
	}
	// The request timeout fits a completion, not this lookup: an endpoint
	// that does not answer must not hold the model back for that long.
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	n, err := client.ContextSize(ctx)
	if err != nil {
		return 0, fmt.Errorf("model %s: discover the context size (set its contextTokens, or LLM_CONTEXT_TOKENS, to skip): %w", m.Name, err)
	}
	return n, nil
}

// probeTimeout bounds the lookup of a model's context size.
const probeTimeout = time.Minute

// newLLM returns the client of model m.
func newLLM(m models.Model) (*llm.Client, error) {
	client, err := llm.New(m.BaseURL, m.APIKey(), m.Model, m.RequestTimeout, llm.WithThinkingBudget(m.ThinkingBudget))
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", m.Name, err)
	}
	return client, nil
}

// jobKinds maps the jobs of the models configuration to job kinds.
var jobKinds = map[string]string{
	models.JobReview:     review.Kind,
	models.JobDiscussion: review.DiscussionKind,
	models.JobChat:       review.ChatKind,
}

// jobModels returns the models of the job queue. Each serves the kinds of
// its jobs this deployment handles (chat only with Zulip), and runs them
// here if it is local and has an endpoint.
func (a *app) jobModels(zulipClient *zulip.Client) ([]jobs.Model, error) {
	enabled := map[string]bool{review.Kind: true, review.DiscussionKind: true, review.ChatKind: zulipClient != nil}
	served, local := map[string]bool{}, map[string]bool{}
	var out []jobs.Model
	for _, m := range a.cfg.Models {
		jm := jobs.Model{Name: m.Name, Concurrency: m.Concurrency}
		for _, j := range m.Jobs {
			if k := jobKinds[j]; enabled[k] {
				jm.Kinds = append(jm.Kinds, k)
				served[k] = true
			}
		}
		if m.Local && m.Runnable() {
			client, err := newLLM(m)
			if err != nil {
				return nil, err
			}
			for _, k := range jm.Kinds {
				local[k] = true
			}
			jm.Handlers = a.handlers(m, client, jm.Kinds, zulipClient)
		}
		out = append(out, jm)
	}
	for _, k := range slices.Sorted(maps.Keys(enabled)) {
		switch {
		case !enabled[k]:
		case !served[k]:
			return nil, fmt.Errorf("no model serves %s jobs: add it to a model's jobs", k)
		case !local[k]:
			a.log.Warn("no local model serves this kind: its jobs wait for runners", "kind", k)
		}
	}
	return out, nil
}

// handlers returns the setup of model m's handlers: once its context size
// is known, a reviewer copy answering with client serves each kind.
func (a *app) handlers(m models.Model, client *llm.Client, kinds []string, zulipClient *zulip.Client) func(context.Context) (map[string]jobs.Handler, error) {
	return func(ctx context.Context) (map[string]jobs.Handler, error) {
		contextTokens, err := connect(ctx, m, client)
		if err != nil {
			return nil, err
		}
		a.log.Info("model ready", "model", m.Name, "base_url", m.BaseURL, "upstream", m.Model,
			"concurrency", m.Concurrency, "context_tokens", contextTokens, "thinking_budget", m.ThinkingBudget, "kinds", kinds)
		// The copy is made now, once serve has set the reviewer's Enqueue.
		r := a.reviewer.WithModel(client, m.Model, contextTokens)
		hs := map[string]jobs.Handler{}
		for _, k := range kinds {
			switch k {
			case review.Kind:
				hs[k] = r
			case review.DiscussionKind:
				hs[k] = r.DiscussionHandler()
			case review.ChatKind:
				hs[k] = &review.Chat{
					Reviewer:      r,
					Zulip:         zulipClient,
					Forges:        a.forges,
					AllowedOwners: a.cfg.AllowedOwners,
					Skill:         a.chatSkill,
				}
			}
		}
		return hs, nil
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := newLogger(cfg.LogLevel, true)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	keys, err := auth.ParseKeys(cfg.APIKeys, cfg.EventsPerMinute)
	if err != nil {
		return fmt.Errorf("API_KEYS: %w", err)
	}
	if len(cfg.AllowedOwners) == 0 {
		return errors.New("ALLOWED_OWNERS is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := newApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	store, err := jobs.OpenStore(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	prometheus.MustRegister(metrics.NewActiveJobsCollector(func(ctx context.Context) ([]metrics.ActiveJobs, error) {
		stats, err := store.Active(ctx)
		out := make([]metrics.ActiveJobs, 0, len(stats))
		for _, s := range stats {
			out = append(out, metrics.ActiveJobs{Status: string(s.Status), Count: s.Count, Oldest: s.Oldest})
		}
		return out, err
	}))
	var zulipClient *zulip.Client
	if cfg.ZulipEnabled() {
		zulipClient = zulip.New(cfg.ZulipURL, cfg.ZulipBotEmail, cfg.ZulipBotAPIKey)
		log.Info("zulip configured", "url", cfg.ZulipURL, "bot", cfg.ZulipBotEmail)
	}
	jobModels, err := a.jobModels(zulipClient)
	if err != nil {
		return err
	}
	queue := jobs.NewQueue(store, jobModels, cfg.JobTimeout, cfg.TranscriptRetention, log)
	a.reviewer.Enqueue = queue.Enqueue

	api := &server.Server{
		Queue: queue, Forges: a.forges, AllowedOwners: cfg.AllowedOwners, Keys: keys,
		Zulip: zulipClient, ZulipToken: cfg.ZulipWebhookToken, Logger: log,
	}
	apiServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}

	forgeURLs := map[string]string{}
	for _, f := range a.forges {
		forgeURLs[f.Name()] = f.ServerURL()
	}
	ui, err := web.New(store, web.Options{Version: version, ForgeURLs: forgeURLs, Logger: log})
	if err != nil {
		return err
	}
	uiServer := &http.Server{
		Addr:              cfg.UIAddr,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	servers := []*http.Server{apiServer, metricsServer, uiServer}

	errs := make(chan error, 1+len(servers))
	var wg sync.WaitGroup
	wg.Go(func() { errs <- queue.Run(ctx) })
	for _, srv := range servers {
		wg.Go(func() {
			log.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
		})
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errs:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("http shutdown", "addr", srv.Addr, "error", err)
		}
	}
	wg.Wait()
	return runErr
}

func reviewOnce(args []string) error {
	fl := flag.NewFlagSet("review", flag.ContinueOnError)
	forgeName := fl.String("forge", "gitea", "forge of the repository: gitea or github")
	repoArg := fl.String("repo", "", "repository, owner/name")
	prNumber := fl.Int("pr", 0, "pull request number")
	headSHA := fl.String("sha", "", "commit to review (default: the pull request head)")
	dryRun := fl.Bool("dry-run", false, "print the review instead of posting it")
	transcript := fl.Bool("transcript", false, "print the agent transcript to stderr")
	modelName := fl.String("model", "", "model to review with (default: the first one serving reviews)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	repo, err := forge.ParseRepo(*repoArg)
	if err != nil {
		return err
	}
	if *prNumber <= 0 {
		return errors.New("-pr is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := newLogger(cfg.LogLevel, false)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := newApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	m, err := reviewModel(cfg.Models, *modelName)
	if err != nil {
		return err
	}
	client, err := newLLM(m)
	if err != nil {
		return err
	}
	contextTokens, err := connect(ctx, m, client)
	if err != nil {
		return err
	}
	log.Info("llm configured", "model", m.Name, "base_url", m.BaseURL, "upstream", m.Model, "context_tokens", contextTokens, "thinking_budget", m.ThinkingBudget)
	a.reviewer = a.reviewer.WithModel(client, m.Model, contextTokens)
	f, ok := a.reviewer.Forges[*forgeName]
	if !ok {
		return fmt.Errorf("forge %q is not configured", *forgeName)
	}
	if *headSHA == "" {
		pr, err := f.GetPR(ctx, repo, *prNumber)
		if err != nil {
			return err
		}
		*headSHA = pr.HeadSHA
	}
	req := review.Request{Forge: f.Name(), Owner: repo.Owner, Repo: repo.Name, PR: *prNumber, HeadSHA: *headSHA}

	record := func(agent.Step) {}
	if *transcript {
		record = printStep
	}
	start := time.Now()
	draft, outcome, err := a.reviewer.Draft(ctx, req, 0, record)
	if err != nil {
		return err
	}
	if draft == nil {
		fmt.Printf("no review: %s\n", outcome)
		return nil
	}
	log.Info("review drafted", "duration", time.Since(start).Round(time.Second), "turns", draft.Turns,
		"prompt_tokens", draft.Usage.PromptTokens, "completion_tokens", draft.Usage.CompletionTokens)
	fmt.Println(draft.Review.Body)
	for _, c := range draft.Review.Comments {
		fmt.Printf("--- inline comment on %s:%d\n%s\n", c.Path, c.Line, c.Body)
	}
	if *dryRun {
		return nil
	}
	outcome, err = a.reviewer.Post(ctx, req, draft)
	if err != nil {
		return err
	}
	fmt.Printf("outcome: %s\n", outcome)
	return nil
}

// reviewModel returns the model named name, or without a name the first
// one with an endpoint serving reviews.
func reviewModel(ms []models.Model, name string) (models.Model, error) {
	for _, m := range ms {
		if name == "" && m.Runnable() && m.Serves(models.JobReview) || name != "" && m.Name == name {
			if !m.Runnable() {
				return models.Model{}, fmt.Errorf("model %s has no baseURL", name)
			}
			return m, nil
		}
	}
	if name != "" {
		return models.Model{}, fmt.Errorf("model %q is not configured", name)
	}
	return models.Model{}, errors.New("no model with a baseURL serves reviews")
}

func printStep(st agent.Step) {
	header := st.Role
	if st.ToolName != "" {
		header += " (" + st.ToolName + ")"
	}
	if st.PromptTokens > 0 {
		header += fmt.Sprintf(" [%d prompt, %d completion tokens]", st.PromptTokens, st.CompletionTokens)
	}
	content := st.Content
	if st.Role == "tool" && len(content) > 2000 {
		content = content[:2000] + "\n[…]"
	}
	fmt.Fprintf(os.Stderr, "\n===== %s =====\n%s\n", header, strings.TrimSpace(content))
}

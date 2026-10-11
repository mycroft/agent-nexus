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
	"net/http"
	"os"
	"os/signal"
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

	client, err := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMRequestTimeout, llm.WithThinkingBudget(cfg.LLMThinkingBudget))
	if err != nil {
		return nil, err
	}
	contextTokens := cfg.LLMContextTokens
	if contextTokens == 0 {
		if contextTokens, err = client.ContextSize(ctx); err != nil {
			return nil, fmt.Errorf("discover the model context size (set LLM_CONTEXT_TOKENS to skip): %w", err)
		}
	}
	log.Info("llm configured", "base_url", cfg.LLMBaseURL, "model", cfg.LLMModel, "context_tokens", contextTokens, "thinking_budget", cfg.LLMThinkingBudget)

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
	a.reviewer = &review.Reviewer{
		Forges:          byName,
		Repos:           repos,
		LLM:             client,
		Model:           cfg.LLMModel,
		Skill:           prSkill,
		DiscussionSkill: discussionSkill,
		ContextTokens:   contextTokens,
		MaxTurns:        cfg.AgentMaxTurns,
		ModeRules:       modeRules,
		Charts:          chartFetcher,
		Remote:          remoteClient,
		FetchHosts:      cfg.FetchAllowedHosts,
		Logger:          log,
	}
	return a, nil
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
	queue := jobs.NewQueue(store, cfg.LLMConcurrency, cfg.JobTimeout, cfg.TranscriptRetention, log)
	a.reviewer.Enqueue = queue.Enqueue
	queue.Register(review.Kind, a.reviewer)
	queue.Register(review.DiscussionKind, a.reviewer.DiscussionHandler())
	var zulipClient *zulip.Client
	if cfg.ZulipEnabled() {
		zulipClient = zulip.New(cfg.ZulipURL, cfg.ZulipBotEmail, cfg.ZulipBotAPIKey)
		queue.Register(review.ChatKind, &review.Chat{
			Reviewer:      a.reviewer,
			Zulip:         zulipClient,
			Forges:        a.forges,
			AllowedOwners: cfg.AllowedOwners,
			Skill:         a.chatSkill,
		})
		log.Info("zulip configured", "url", cfg.ZulipURL, "bot", cfg.ZulipBotEmail)
	}

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

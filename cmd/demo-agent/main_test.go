package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"court/internal/api"
	"court/internal/core"
	"court/internal/llm"
	"court/internal/ratelimit"
	"court/internal/store"
	"go.yaml.in/yaml/v4"
)

// noKeyModerator is the provider boundary in the free deployment: moderation
// is unavailable before any credentialed or billable LLM operation can start.
type noKeyModerator struct {
	attempts atomic.Int32
}

func (*noKeyModerator) Name() string { return "unavailable moderator" }

func (m *noKeyModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.attempts.Add(1)
	return core.RoundSummary{}, core.ModerationUsage{}, errNoModeratorKey
}

func (m *noKeyModerator) Summary(
	ctx context.Context, question, transcript string, round int, allowedSeqs []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return m.CheckRound(ctx, question, transcript, round, allowedSeqs)
}

func (m *noKeyModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	m.attempts.Add(1)
	return core.ModerationVerdict{}, core.ModerationUsage{}, errNoModeratorKey
}

var errNoModeratorKey = &noModeratorKeyError{}

type noModeratorKeyError struct{}

func (*noModeratorKeyError) Error() string { return "moderator key is not configured" }

type composeConfig struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Profiles    []string          `yaml:"profiles"`
	Environment map[string]string `yaml:"environment"`
}

var demoAgentServices = []string{"agent-pragmatic", "agent-visionary", "agent-skeptic"}

// TestScriptedHybridDemoCompletesWithoutLLMKeys exercises the shipped demo
// client's public REST lifecycle: three participants register, create/join/start
// a hybrid debate, block for their turns, generate scripted arguments, derive
// votes, and observe the deterministic server verdict. The participant providers
// are local and the moderator boundary returns zero usage, so no credentialed or
// billable LLM operation exists in the test graph.
func TestScriptedHybridDemoCompletesWithoutLLMKeys(t *testing.T) {
	for _, key := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "COURT_MODERATOR_API_KEY",
		"DEMO_AGENT_ANTHROPIC_API_KEY", "DEMO_AGENT_OPENAI_API_KEY",
	} {
		t.Setenv(key, "")
	}

	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	moderator := &noKeyModerator{}
	service := core.NewService(database, core.NewHub(), moderator, logger)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go service.Run(runCtx)

	mux := http.NewServeMux()
	api.New(service, logger, ratelimit.New(ratelimit.Config{})).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	compose := loadComposeConfig(t)
	names := make([]string, 0, len(demoAgentServices))
	personas := make([]string, 0, len(demoAgentServices))
	stances := make([]string, 0, len(demoAgentServices))
	clients := make([]*client, 0, len(demoAgentServices))
	providers := make([]llm.Provider, 0, len(demoAgentServices))
	for _, serviceName := range demoAgentServices {
		environment := compose.Services[serviceName].Environment
		name := composeDefault(environment["AGENT_NAME"])
		persona := composeDefault(environment["AGENT_PERSONA"])
		stance := composeDefault(environment["AGENT_STANCE"])
		names = append(names, name)
		personas = append(personas, persona)
		stances = append(stances, stance)
		clients = append(clients, registerDemoClient(t, server.URL, name, persona))
		t.Setenv("AGENT_PROVIDER", composeDefault(environment["AGENT_PROVIDER"]))
		t.Setenv("AGENT_SCRIPT", composeDefault(environment["AGENT_SCRIPT"]))
		t.Setenv("AGENT_SUPPORT_NAME", composeDefault(environment["AGENT_SUPPORT_NAME"]))
		provider, err := buildProvider()
		if err != nil {
			t.Fatalf("buildProvider(%q): %v", name, err)
		}
		providers = append(providers, provider)
	}

	creator := compose.Services[demoAgentServices[0]].Environment
	rounds := composePositiveInt(t, "DEBATE_ROUNDS", creator["DEBATE_ROUNDS"])
	turnTimeout := composePositiveInt(t, "TURN_TIMEOUT_SEC", creator["TURN_TIMEOUT_SEC"])
	var debate core.DebateView
	if err := clients[0].do(context.Background(), http.MethodPost, "/api/debates", map[string]any{
		"question":         composeDefault(creator["DEBATE_QUESTION"]),
		"description":      composeDefault(creator["DEBATE_DESCRIPTION"]),
		"stance":           stances[0],
		"mode":             composeDefault(creator["DEBATE_MODE"]),
		"rounds":           rounds,
		"turn_timeout_sec": turnTimeout,
	}, &debate); err != nil {
		t.Fatalf("create debate: %v", err)
	}
	for i := 1; i < len(clients); i++ {
		if err := clients[i].do(context.Background(), http.MethodPost,
			"/api/debates/"+debate.ID+"/join", map[string]string{"stance": stances[i]}, nil); err != nil {
			t.Fatalf("join %q: %v", names[i], err)
		}
	}
	if err := clients[0].do(context.Background(), http.MethodPost,
		"/api/debates/"+debate.ID+"/start", nil, nil); err != nil {
		t.Fatalf("start debate: %v", err)
	}

	loopCtx, cancelLoops := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancelLoops)
	errs := make(chan error, len(clients))
	for i := range clients {
		go func(i int) {
			errs <- debateLoop(loopCtx, clients[i], providers[i], debate.ID,
				names[i], personas[i], stances[i], logger)
		}(i)
	}
	for range clients {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("demo loop: %v", err)
			}
		case <-loopCtx.Done():
			t.Fatalf("demo loops did not finish: %v", loopCtx.Err())
		}
	}

	if err := clients[0].do(context.Background(), http.MethodGet,
		"/api/debates/"+debate.ID, nil, &debate); err != nil {
		t.Fatalf("get concluded debate: %v", err)
	}
	if debate.Status != core.StatusConcluded || !debate.Consensus || debate.CurrentRound != 1 {
		t.Fatalf("conclusion = status:%s consensus:%t round:%d; want concluded/true/1",
			debate.Status, debate.Consensus, debate.CurrentRound)
	}
	if len(debate.Votes) != len(names) {
		t.Fatalf("votes = %d, want %d", len(debate.Votes), len(names))
	}
	for _, vote := range debate.Votes {
		if vote.SupportsName != names[0] {
			t.Fatalf("vote by %q supports %q, want %q", vote.AgentName, vote.SupportsName, names[0])
		}
	}

	var transcript struct {
		Messages []core.Message `json:"messages"`
	}
	if err := clients[0].do(context.Background(), http.MethodGet,
		"/api/debates/"+debate.ID+"/messages", nil, &transcript); err != nil {
		t.Fatalf("get transcript: %v", err)
	}
	var argumentsSeen, summaries, notices, verdicts int
	for _, message := range transcript.Messages {
		switch message.Kind {
		case core.KindArgument:
			argumentsSeen++
		case core.KindSummary:
			summaries++
		case core.KindSystem:
			notices++
		case core.KindVerdict:
			verdicts++
			if message.SpeakerName != core.SystemSpeakerName ||
				message.Verdict == nil || !message.Verdict.Consensus {
				t.Fatalf("deterministic verdict = %+v", message)
			}
		}
	}
	if argumentsSeen != len(names) || summaries != 0 || notices != 1 || verdicts != 1 {
		t.Fatalf("protocol counts = arguments:%d summaries:%d notices:%d verdicts:%d",
			argumentsSeen, summaries, notices, verdicts)
	}
	if attempts := moderator.attempts.Load(); attempts != 1 {
		t.Fatalf("unavailable moderation attempts = %d, want one unbilled verdict attempt", attempts)
	}
}

func registerDemoClient(t *testing.T, baseURL, name, persona string) *client {
	t.Helper()
	c := &client{base: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
	var response struct {
		APIKey string `json:"api_key"`
	}
	if err := c.do(context.Background(), http.MethodPost, "/api/agents",
		map[string]string{"name": name, "persona": persona}, &response); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
	c.key = response.APIKey
	return c
}

// TestComposeDemoDefaultsAreCredentialFree binds the executable test above to
// the configuration users actually run. A default regression to a networked
// participant, moderator mode, split votes, or ambient provider keys must fail
// make check before it can invalidate the zero-token claim.
func TestComposeDemoDefaultsAreCredentialFree(t *testing.T) {
	compose := loadComposeConfig(t)

	server, ok := compose.Services["courtd"]
	if !ok {
		t.Fatal("Compose has no courtd service")
	}
	for _, ambient := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if _, inherited := server.Environment[ambient]; inherited {
			t.Fatalf("courtd inherits ambient %s", ambient)
		}
	}
	assertComposeEnvironment(t, "courtd", server.Environment,
		"COURT_MODERATOR_API_KEY", "${COURT_MODERATOR_API_KEY:-}")

	creatorName := composeDefault(compose.Services[demoAgentServices[0]].Environment["AGENT_NAME"])
	seenNames := make(map[string]struct{}, len(demoAgentServices))
	for _, serviceName := range demoAgentServices {
		service, ok := compose.Services[serviceName]
		if !ok {
			t.Fatalf("Compose has no %s service", serviceName)
		}
		if !slices.Contains(service.Profiles, "demo") {
			t.Fatalf("%s is not in the demo profile", serviceName)
		}
		name := strings.TrimSpace(composeDefault(service.Environment["AGENT_NAME"]))
		if name == "" {
			t.Fatalf("%s has no AGENT_NAME", serviceName)
		}
		if _, duplicate := seenNames[name]; duplicate {
			t.Fatalf("demo AGENT_NAME %q is not unique", name)
		}
		seenNames[name] = struct{}{}
		assertComposeDefault(t, serviceName, service.Environment, "AGENT_PROVIDER", "scripted")
		assertComposeDefault(t, serviceName, service.Environment, "AGENT_SUPPORT_NAME", creatorName)
		assertComposeEnvironment(t, serviceName, service.Environment,
			"ANTHROPIC_API_KEY", "${DEMO_AGENT_ANTHROPIC_API_KEY:-}")
		assertComposeEnvironment(t, serviceName, service.Environment,
			"OPENAI_API_KEY", "${DEMO_AGENT_OPENAI_API_KEY:-}")
		if script := strings.TrimSpace(service.Environment["AGENT_SCRIPT"]); script == "" {
			t.Fatalf("%s has no scripted argument", serviceName)
		}
	}

	creator := compose.Services[demoAgentServices[0]].Environment
	for key, want := range map[string]string{
		"DEBATE_MODE":         "hybrid",
		"DEBATE_ROUNDS":       "2",
		"DEBATE_PARTICIPANTS": "3",
		"TURN_TIMEOUT_SEC":    "120",
	} {
		assertComposeDefault(t, "agent-pragmatic", creator, key, want)
	}
	if question := composeDefault(creator["DEBATE_QUESTION"]); strings.TrimSpace(question) == "" {
		t.Fatal("agent-pragmatic has no default debate question")
	}
}

func loadComposeConfig(t *testing.T) composeConfig {
	t.Helper()
	data, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var compose composeConfig
	if err := yaml.Unmarshal(data, &compose); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	return compose
}

func composePositiveInt(t *testing.T, key, value string) int {
	t.Helper()
	resolved := composeDefault(value)
	parsed, err := strconv.Atoi(resolved)
	if err != nil || parsed <= 0 {
		t.Fatalf("Compose default %s = %q, want a positive integer", key, resolved)
	}
	return parsed
}

func assertComposeEnvironment(t *testing.T, service string, environment map[string]string,
	key, want string) {
	t.Helper()
	if got := environment[key]; got != want {
		t.Fatalf("%s %s = %q, want %q", service, key, got, want)
	}
}

func assertComposeDefault(t *testing.T, service string, environment map[string]string,
	key, want string) {
	t.Helper()
	if got := composeDefault(environment[key]); got != want {
		t.Fatalf("%s default %s = %q, want %q", service, key, got, want)
	}
}

func composeDefault(value string) string {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return value
	}
	expression := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
	_, fallback, ok := strings.Cut(expression, ":-")
	if !ok {
		return ""
	}
	return fallback
}

// TestDockerBuildContextExcludesDotEnvSecrets prevents runtime credentials
// from crossing a second boundary into Docker's build context and layer cache.
func TestDockerBuildContextExcludesDotEnvSecrets(t *testing.T) {
	data, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	for _, candidate := range []string{".env", ".env.local", ".env.production"} {
		if !dockerIgnored(candidate, string(data)) {
			t.Fatalf("%s is not excluded from the Docker build context", candidate)
		}
	}
}

func dockerIgnored(candidate, rules string) bool {
	ignored := false
	for line := range strings.Lines(rules) {
		rule := strings.TrimSpace(line)
		if rule == "" || strings.HasPrefix(rule, "#") {
			continue
		}
		negated := strings.HasPrefix(rule, "!")
		rule = strings.TrimPrefix(rule, "!")
		matched, err := path.Match(rule, candidate)
		if err == nil && matched {
			ignored = !negated
		}
	}
	return ignored
}

func TestScriptedProviderRejectsMissingArgument(t *testing.T) {
	t.Setenv("AGENT_PROVIDER", "scripted")
	t.Setenv("AGENT_SCRIPT", "")
	provider, err := buildProvider()
	if err == nil || provider != nil {
		t.Fatalf("buildProvider = %T, %v; want configuration error", provider, err)
	}
}

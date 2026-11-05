package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/memory"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/plan"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// conversationSession is the host's persistence/execution boundary. Persistent
// and process-owning sessions compose the public harness; other sessions retain
// agentcore.Session. This permits indexed recovery and decorating Monitor BEFORE
// registry creation, with the same official Manager for tools and notifications.
// No SDK internals, unsafe access, replacement source or alternate loop is used.
type conversationSession interface {
	Prompt(context.Context, string) iter.Seq2[harness.Event, error]
	Resume(string) error
	Close() error
	Messages() []llm.Message
	Usage() llm.Usage
	SessionID() string
	UnlockSet() *actool.UnlockSet
	Reset()
}

type hostedSession struct {
	opts      agentcore.Options
	registry  *actool.Registry
	compactor harness.Compactor
	plan      *plan.Controller
	tasks     *actool.Manager
	ptys      *ptyOwner
	unlock    *actool.UnlockSet
	id        string
	writer    *transcript.Writer
	messages  []llm.Message
	usage     llm.Usage
	closeOnce sync.Once
	closeErr  error
	recovery  *recoveryJournal
}

func newHostedSession(opts agentcore.Options, hooks *dispatchHooks) (*hostedSession, error) {
	if opts.PermissionMode == "" {
		opts.PermissionMode = permission.ModeDefault
	}
	id := opts.SessionID
	if id == "" {
		id = transcript.NewSessionID()
	}
	var recovery *recoveryJournal
	if opts.Transcript != nil {
		var err error
		recovery, err = newRecoveryJournal(opts.Transcript.MainPath(id), id)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errSessionRecovery, err)
		}
	}
	var manager *actool.Manager
	background := !opts.DisableBackgroundTasks && !actool.BackgroundTasksDisabled()
	if background {
		var err error
		manager, err = actool.NewManager(id)
		if err != nil {
			if recovery != nil {
				recovery.Close()
			}
			return nil, err
		}
	}
	s := &hostedSession{opts: opts, id: id, tasks: manager, ptys: hooks.ptys, recovery: recovery}
	if opts.Compactor != nil {
		s.compactor = opts.Compactor
		if opts.Compaction != nil && opts.OnWarn != nil {
			opts.OnWarn("agentcore: both Compactor and Compaction are set — Compaction is IGNORED. They are mutually exclusive context managers; set only one.")
		}
	} else if opts.Compaction != nil {
		s.compactor = compaction.New(*opts.Compaction, compaction.ProviderSummarizer(compactionUsageProvider{opts.Provider}, opts.MaxTokens, opts.NonStreaming))
	}
	tools := append([]actool.CoreTool{}, opts.Tools...)
	if opts.Plan != nil {
		mode := opts.PermissionMode
		if opts.Plan.StartInPlanMode {
			mode = permission.ModePlan
		}
		s.plan = plan.NewController(mode, opts.Plan.TargetMode)
		tools = append(tools, plan.Tools(s.plan, opts.Plan.Approver)...)
	}
	if opts.Memory != nil && opts.Memory.Store != nil {
		tools = append(tools, memory.Tools(opts.Memory.Store)...)
	}
	if opts.Skills != nil {
		tools = append(tools, opts.Skills.Tool())
	}
	if opts.EnableWebFetch {
		tools = append(tools, actool.NewWebFetch(actool.WebFetchConfig{Proxy: opts.WebFetchProxy, CACert: opts.WebFetchCACert, InsecureTLS: opts.WebFetchInsecureTLS}))
	}
	if opts.EnableWebSearch {
		ws, err := actool.NewWebSearch(actool.WebSearchConfig{Backend: opts.WebSearchBackend, BraveAPIKey: opts.BraveSearchAPIKey, TavilyAPIKey: opts.TavilySearchAPIKey, DeepSeekBaseURL: opts.DeepSeekSearchBaseURL, DeepSeekAPIKey: opts.DeepSeekSearchAPIKey, DeepSeekModel: opts.DeepSeekSearchModel, Proxy: opts.WebSearchProxy, CACert: opts.WebSearchCACert, InsecureTLS: opts.WebSearchInsecureTLS})
		if err == nil {
			tools = append(tools, ws)
		} // preserve official invalid-backend behavior
	}
	if opts.Todos != nil {
		tools = append(tools, opts.Todos.Tool())
	}
	if opts.AskUser != nil {
		tools = append(tools, actool.NewAskUserQuestion(opts.AskUser))
	}
	if background {
		monitor := &processContextTool{CoreTool: newAdmittedMonitor(), hooks: hooks, background: true}
		tools = append(tools, actool.NewTaskOutput(), actool.NewTaskStop(), actool.NewTaskList(), monitor)
	}
	if s.ptys != nil {
		for i, t := range tools {
			tools[i] = &ptyNoticeTool{CoreTool: t, owner: s.ptys}
		}
	}
	s.registry = actool.NewRegistry(tools...)
	if len(opts.DeferredTools) > 0 {
		s.unlock = opts.UnlockSet
		if s.unlock == nil {
			s.unlock = actool.NewUnlockSet(opts.DeferredTools...)
		}
		s.registry.Add(actool.NewSearchExtraTools(s.registry, opts.DeferredTools))
		s.registry.Add(actool.NewExecuteExtraTool(s.registry, s.unlock))
	}
	if opts.Transcript != nil {
		s.writer = opts.Transcript.NewWriter(id, "")
	}
	return s, nil
}

func (s *hostedSession) Close() error {
	s.closeOnce.Do(func() {
		if s.ptys != nil {
			s.closeErr = s.ptys.closeWait(5 * time.Second)
			if s.closeErr != nil && s.opts.OnWarn != nil {
				s.opts.OnWarn(s.closeErr.Error())
			}
		}
		s.tasks.Cleanup()
		if s.recovery != nil {
			if err := s.recovery.Close(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}
func (s *hostedSession) Messages() []llm.Message      { return s.messages }
func (s *hostedSession) Usage() llm.Usage             { return s.usage }
func (s *hostedSession) SessionID() string            { return s.id }
func (s *hostedSession) UnlockSet() *actool.UnlockSet { return s.unlock }
func (s *hostedSession) Reset()                       { s.messages = nil; s.usage = llm.Usage{} }

func (s *hostedSession) Resume(id string) error {
	return s.ResumeContext(context.Background(), id)
}

// Preserve official post-boundary contents, usage and incomplete trailing-batch
// handling, while seeking to the durable boundary rather than materializing all
// old records first. Pure noa histories without an SDK boundary remain intact.
func (s *hostedSession) ResumeContext(ctx context.Context, id string) error {
	if s.opts.Transcript == nil {
		return errors.New("agentcore: Resume requires Options.Transcript")
	}
	if s.recovery == nil || s.recovery.id != id {
		next, err := newRecoveryJournal(s.opts.Transcript.MainPath(id), id)
		if err != nil {
			return err
		}
		if s.recovery != nil {
			s.recovery.Close()
		}
		s.recovery = next
	}
	msgs, usage, last, err := s.recovery.Restore(ctx)
	if err != nil {
		return err
	}
	metrics := s.recovery.metrics
	log.Printf("[session-recovery] session=%q raw_bytes=%d indexed_new_bytes=%d verified_bytes=%d working_bytes=%d working_messages=%d sdk_boundary=%t", id, s.recovery.head.Size, metrics.IndexedBytes, metrics.VerifiedBytes, metrics.RestoredBytes, metrics.Messages, s.recovery.head.Boundary)
	s.messages, s.usage, s.id = msgs, usage, id
	s.writer = s.opts.Transcript.ResumeWriter(id, last)
	return nil
}

func (s *hostedSession) HasWorkerRequest(ctx context.Context, id string) (bool, error) {
	if s.recovery == nil {
		return hasWorkerChatMessage(s.Messages(), id), nil
	}
	return s.recovery.HasWorkerRequest(ctx, id)
}
func (s *hostedSession) ReplayRecoveredSkills(ctx context.Context, unlock func(string)) error {
	if s.recovery == nil {
		return nil
	}
	return s.recovery.ReplaySkills(ctx, unlock)
}

func (s *hostedSession) record(message llm.Message) {
	s.messages = append(s.messages, message)
	if s.writer != nil {
		s.writer.RecordMessage(message, llm.Usage{})
	}
}

func (s *hostedSession) Prompt(ctx context.Context, input string) iter.Seq2[harness.Event, error] {
	opts := s.opts
	if m := opts.Memory; m != nil && m.Store != nil && m.AutoInject {
		max := m.MaxInject
		if max <= 0 {
			max = 3
		}
		if rows := m.Store.Relevant(input, max); len(rows) > 0 {
			input += "\n\n" + memory.RenderForInjection(rows, time.Now())
		}
	}
	if hooks, ok := opts.Hooks.(interface {
		FireSessionStart(context.Context)
		FireUserPromptSubmit(context.Context, string) string
	}); ok {
		if len(s.messages) == 0 {
			hooks.FireSessionStart(ctx)
		}
		if extra := hooks.FireUserPromptSubmit(ctx, input); extra != "" {
			input += "\n\n" + extra
		}
	}
	var notes []actool.Notification
	if s.tasks != nil {
		notes = s.tasks.DrainNotifications()
	}
	notes = append(notes, s.ptys.drain()...)
	if len(notes) > 0 {
		var text strings.Builder
		for i, note := range notes {
			if i > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(note.String())
		}
		s.record(llm.UserText(text.String()))
	}
	s.record(llm.UserText(input))
	if s.writer != nil {
		ctx = transcript.WithSessionID(ctx, s.id)
	}
	system := append(append([]string(nil), opts.SystemPrompt...), opts.AppendSystemPrompt...)
	if m := opts.Memory; m != nil && m.Store != nil && m.IncludeIndexInPrompt {
		system = append(system, m.Store.SystemSegment())
	}
	reminder := "<system-reminder>\n# currentDate\nToday's date is " + time.Now().Format("2006-01-02") + "."
	if opts.SystemReminderFunc != nil {
		if extra := opts.SystemReminderFunc(); extra != "" {
			reminder += "\n" + extra
		}
	}
	reminder += "\n</system-reminder>"
	query := harness.QueryInput{
		Provider: opts.Provider, System: system, DynamicBoundary: opts.DynamicBoundary, Messages: s.messages, SystemReminder: reminder,
		Tools: s.registry, DeferredTools: opts.DeferredTools, MaxTokens: opts.MaxTokens, Temperature: opts.Temperature,
		MaxTurns: opts.MaxTurns, MaxDuration: opts.MaxDuration, Settlement: opts.Settlement, MaxConcurrency: opts.MaxConcurrency,
		WorkingDir: opts.WorkingDir, Todos: opts.Todos, ToolOutputDir: opts.ToolOutputDir, MaxToolOutputChars: opts.MaxToolOutputChars,
		Tasks: s.tasks, BashEnv: opts.BashEnv, PermissionMode: opts.PermissionMode, Allowed: opts.AllowedTools, Disallowed: opts.DisallowedTools,
		CanUseTool: opts.CanUseTool, Hooks: opts.Hooks, Compactor: s.compactor, TokenBudget: opts.TokenBudget,
		EscalateMaxTokens: opts.EscalateMaxTokens, NonStreaming: opts.NonStreaming,
	}
	if s.writer != nil {
		query.Recorder = s.writer
	}
	if s.plan != nil {
		query.PermissionModeFunc = s.plan.Mode
	}
	return func(yield func(harness.Event, error) bool) {
		if s.recovery != nil {
			defer func() {
				flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := s.recovery.Sync(flushCtx); err != nil {
					log.Printf("[session-recovery] session=%q index_update_failed=true error_type=%T", s.id, err)
					if s.opts.OnWarn != nil {
						s.opts.OnWarn("恢复索引更新失败；原始会话仍保留，下次恢复将重试。")
					}
				}
			}()
		}
		if s.ptys != nil {
			s.runPTYQuery(ctx, query, opts.Deps, yield)
			return
		}
		for event, err := range harness.Query(ctx, query, opts.Deps) {
			if event.Kind == harness.KindResult && event.Terminal != nil {
				s.messages, s.usage = append([]llm.Message(nil), llm.MessagesAfterBoundary(event.Terminal.Messages)...), event.Terminal.Usage
			}
			if !yield(event, err) {
				return
			}
		}
	}
}

var _ conversationSession = (*agentcore.Session)(nil)
var _ conversationSession = (*hostedSession)(nil)

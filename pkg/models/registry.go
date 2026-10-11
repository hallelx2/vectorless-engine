package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hallelx2/llmgate"
)

// SettingKey is the instance_settings row that holds model providers.
const SettingKey = "models"

// aad binds the sealed row to its purpose.
const aad = "vectorless/instance_settings/" + SettingKey

// StoredValue is one sealed row as the store returns it.
type StoredValue struct {
	Value     []byte
	UpdatedAt time.Time
	UpdatedBy string
}

// Store persists sealed settings. *db.Pool satisfies it through
// DBStore; tests use an in-memory one.
type Store interface {
	// Get returns the row, or ok=false when there is none.
	Get(ctx context.Context) (v StoredValue, ok bool, err error)
	// Put writes the row and returns its new timestamp.
	Put(ctx context.Context, value []byte, updatedBy string) (time.Time, error)
	// Delete removes the row.
	Delete(ctx context.Context) error
	// UpdatedAt is the cheap poll: the zero time when there is no row.
	UpdatedAt(ctx context.Context) (time.Time, error)
}

// ClientBuilder builds a generative client from settings.
type ClientBuilder func(Generative) (llmgate.Client, error)

// JudgeBuilder builds a Judge from settings.
type JudgeBuilder func(JudgeSettings) (llmgate.Judge, error)

// Options configures a Registry.
type Options struct {
	// Env is what the environment and config file configure; either
	// group may be nil.
	Env Settings
	// Store holds the dashboard's settings. Nil disables them.
	Store Store
	// Cipher seals them. Nil refuses to store keys.
	Cipher *Cipher
	// BuildClient defaults to BuildClient.
	BuildClient ClientBuilder
	// BuildJudge defaults to BuildJudge.
	BuildJudge JudgeBuilder
	Logger     *slog.Logger
}

// state is one immutable snapshot of the effective providers.
type state struct {
	gen       *Generative
	genSource Source
	genErr    error
	client    llmgate.Client

	judge       *JudgeSettings
	judgeSource Source
	judgeErr    error
	judgeClient llmgate.Judge

	updatedAt time.Time
	updatedBy string
	storeErr  error
}

// Registry holds the effective model providers and swaps them in place.
type Registry struct {
	opts Options
	cur  atomic.Pointer[state]

	// mu serialises reloads and writes so two saves cannot interleave.
	mu sync.Mutex

	client *proxyClient
	judge  *proxyJudge
}

// New builds a Registry from the environment alone. Call Reload to read
// the dashboard's settings, and Watch to follow changes.
func New(opts Options) *Registry {
	if opts.BuildClient == nil {
		opts.BuildClient = BuildClient
	}
	if opts.BuildJudge == nil {
		opts.BuildJudge = BuildJudge
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &Registry{opts: opts}
	r.client = &proxyClient{r: r}
	r.judge = &proxyJudge{r: r}
	r.cur.Store(r.build(Settings{}, StoredValue{}, nil))
	return r
}

// Client is the generative client every caller should hold. It routes
// each call to the provider configured at that moment.
func (r *Registry) Client() llmgate.Client { return r.client }

// Judge is the Judge every caller should hold. Pass it through
// ActiveJudge where a nil Judge selects a different path.
func (r *Registry) Judge() llmgate.Judge { return r.judge }

// GenerativeConfigured reports whether a generative model is usable now.
func (r *Registry) GenerativeConfigured() bool { return r.cur.Load().client != nil }

// JudgeConfigured reports whether the Judge is usable now.
func (r *Registry) JudgeConfigured() bool { return r.cur.Load().judgeClient != nil }

// AnswerModel is the model that writes answers, or "" with no provider.
func (r *Registry) AnswerModel() string {
	s := r.cur.Load()
	if s.gen == nil || s.client == nil {
		return ""
	}
	return s.gen.ResolvedAnswerModel()
}

// IngestModel is the default model for every other call.
func (r *Registry) IngestModel() string {
	s := r.cur.Load()
	if s.gen == nil || s.client == nil {
		return ""
	}
	return s.gen.ResolvedIngestModel()
}

// Status is the redacted view for the dashboard.
func (r *Registry) Status() Status {
	s := r.cur.Load()
	st := Status{
		EncryptionConfigured: r.opts.Cipher != nil,
		UpdatedAt:            s.updatedAt,
		UpdatedBy:            s.updatedBy,
	}
	if s.storeErr != nil {
		st.StoreError = s.storeErr.Error()
	}
	st.Generative = GenerativeStatus{Source: s.genSource, EnvAvailable: r.opts.Env.Generative != nil, Configured: s.client != nil}
	if s.gen != nil {
		st.Generative.Provider = s.gen.Provider
		st.Generative.BaseURL = s.gen.BaseURL
		st.Generative.AnswerModel = s.gen.ResolvedAnswerModel()
		st.Generative.IngestModel = s.gen.ResolvedIngestModel()
		st.Generative.KeyHint = keyHint(s.gen.APIKey)
	}
	if s.genErr != nil {
		st.Generative.Error = s.genErr.Error()
	}
	st.Judge = JudgeStatus{Source: s.judgeSource, EnvAvailable: r.opts.Env.Judge != nil, Configured: s.judgeClient != nil}
	if s.judge != nil {
		st.Judge.BaseURL = s.judge.BaseURL
		st.Judge.Model = s.judge.Model
		st.Judge.KeyHint = keyHint(s.judge.APIKey)
	}
	if s.judgeErr != nil {
		st.Judge.Error = s.judgeErr.Error()
	}
	return st
}

// Reload reads the dashboard's settings and swaps in new providers. A
// store that cannot be read keeps the environment's providers and
// reports why in Status.
func (r *Registry) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked(ctx)
}

func (r *Registry) reloadLocked(ctx context.Context) error {
	stored, row, err := r.readStored(ctx)
	r.cur.Store(r.build(stored, row, err))
	return err
}

func (r *Registry) readStored(ctx context.Context) (Settings, StoredValue, error) {
	if r.opts.Store == nil {
		return Settings{}, StoredValue{}, nil
	}
	row, ok, err := r.opts.Store.Get(ctx)
	if err != nil {
		return Settings{}, StoredValue{}, fmt.Errorf("models: read settings: %w", err)
	}
	if !ok {
		return Settings{}, StoredValue{}, nil
	}
	var s Settings
	plain, err := r.opts.Cipher.Open(row.Value, aad)
	if err != nil {
		return Settings{}, row, err
	}
	if err := json.Unmarshal(plain, &s); err != nil {
		return Settings{}, row, fmt.Errorf("models: decode settings: %w", err)
	}
	return s, row, nil
}

// build merges stored over the environment and constructs clients.
func (r *Registry) build(stored Settings, row StoredValue, storeErr error) *state {
	s := &state{genSource: SourceNone, judgeSource: SourceNone, updatedAt: row.UpdatedAt, updatedBy: row.UpdatedBy, storeErr: storeErr}

	switch {
	case stored.Generative != nil:
		s.gen, s.genSource = stored.Generative, SourceUI
	case r.opts.Env.Generative != nil:
		s.gen, s.genSource = r.opts.Env.Generative, SourceEnv
	}
	if s.gen != nil {
		if err := s.gen.Validate(); err != nil {
			s.genErr = err
		} else if c, err := r.opts.BuildClient(*s.gen); err != nil {
			s.genErr = err
		} else {
			s.client = c
		}
	}

	switch {
	case stored.Judge != nil:
		s.judge, s.judgeSource = stored.Judge, SourceUI
	case r.opts.Env.Judge != nil:
		s.judge, s.judgeSource = r.opts.Env.Judge, SourceEnv
	}
	if s.judge != nil {
		if err := s.judge.Validate(); err != nil {
			s.judgeErr = err
		} else if j, err := r.opts.BuildJudge(*s.judge); err != nil {
			s.judgeErr = err
		} else {
			s.judgeClient = j
		}
	}
	return s
}

// Save applies an update from the dashboard, stores it sealed, and swaps
// the providers in before returning — the next call uses them.
func (r *Registry) Save(ctx context.Context, u Update, by string) (Status, error) {
	if r.opts.Store == nil {
		return Status{}, errors.New("models: this engine has no settings store")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, _, err := r.readStored(ctx)
	if err != nil && !errors.Is(err, ErrSealedCorrupt) {
		return Status{}, err
	}
	// A row sealed under a lost key cannot be merged; the update replaces it.
	next, err := u.apply(stored)
	if err != nil {
		return Status{}, &ValidationError{Err: err}
	}
	if next.Generative == nil && next.Judge == nil {
		if err := r.opts.Store.Delete(ctx); err != nil {
			return Status{}, fmt.Errorf("models: clear settings: %w", err)
		}
	} else {
		if r.opts.Cipher == nil {
			return Status{}, ErrNoEncryptionKey
		}
		plain, err := json.Marshal(next)
		if err != nil {
			return Status{}, err
		}
		sealed, err := r.opts.Cipher.Seal(plain, aad)
		if err != nil {
			return Status{}, err
		}
		if _, err := r.opts.Store.Put(ctx, sealed, by); err != nil {
			return Status{}, fmt.Errorf("models: store settings: %w", err)
		}
	}
	if err := r.reloadLocked(ctx); err != nil {
		return Status{}, err
	}
	r.opts.Logger.Info("models: settings saved", "by", by,
		"generative_source", r.cur.Load().genSource, "judge_source", r.cur.Load().judgeSource)
	return r.Status(), nil
}

// ValidationError is an update the caller must fix.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Watch polls the store every interval and reloads when the settings
// changed — another replica saved them, or someone edited the row. It
// returns when ctx ends.
func (r *Registry) Watch(ctx context.Context, interval time.Duration) {
	if r.opts.Store == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		at, err := r.opts.Store.UpdatedAt(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.opts.Logger.Warn("models: poll settings", "err", err)
			}
			continue
		}
		if at.Equal(r.cur.Load().updatedAt) {
			continue
		}
		if err := r.Reload(ctx); err != nil {
			r.opts.Logger.Warn("models: reload settings", "err", err)
			continue
		}
		r.opts.Logger.Info("models: settings changed; providers reloaded")
	}
}

// ── Proxies ──────────────────────────────────────────────────────────

type proxyClient struct{ r *Registry }

func (p *proxyClient) current() (llmgate.Client, error) {
	s := p.r.cur.Load()
	if s.client == nil {
		return nil, &NotConfiguredError{Kind: "generative", Cause: s.genErr}
	}
	return s.client, nil
}

func (p *proxyClient) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	c, err := p.current()
	if err != nil {
		return nil, err
	}
	return c.Complete(ctx, req)
}

func (p *proxyClient) CountTokens(ctx context.Context, text string) (int, error) {
	c, err := p.current()
	if err != nil {
		// Token counting is an estimate the callers can do without a
		// provider; keep it working so budgeting never needs a key.
		return len(text) / 4, nil
	}
	return c.CountTokens(ctx, text)
}

type proxyJudge struct{ r *Registry }

func (p *proxyJudge) Judge(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
	s := p.r.cur.Load()
	if s.judgeClient == nil {
		return nil, &NotConfiguredError{Kind: "judge", Cause: s.judgeErr}
	}
	return s.judgeClient.Judge(ctx, req)
}

// Active reports the Judge to use now: the proxy itself while a Judge is
// configured, nil otherwise.
func (p *proxyJudge) Active() llmgate.Judge {
	if p.r.cur.Load().judgeClient == nil {
		return nil
	}
	return p
}

// ActiveJudge returns j, or nil when j is a Registry's Judge with no
// provider configured right now. Code that chooses a path on a nil Judge
// calls it once per job or request, so it sees one consistent answer.
func ActiveJudge(j llmgate.Judge) llmgate.Judge {
	if a, ok := j.(interface{ Active() llmgate.Judge }); ok {
		return a.Active()
	}
	return j
}

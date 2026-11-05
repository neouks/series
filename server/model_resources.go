package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/neouks/series/runtimebudget"
)

const settingModelRequestLimits = "model_request_limits"
const settingModelRequestDeadlines = "model_request_deadlines"
const settingModelByteLimits = "model_request_bytes"

type modelRequestLimits = runtimebudget.Limits

func modelRequestResourceSnapshot() runtimebudget.Snapshot {
	return runtimebudget.ModelGate().Snapshot()
}

func (m *Manager) loadModelRequestLimits() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, found, err := m.pg.GetSettingContext(ctx, settingModelRequestLimits)
	if err != nil {
		return err
	}
	limits := runtimebudget.Limits{Concurrency: 8, Queue: 256}
	if found {
		limits = runtimebudget.Limits{}
		if err = json.Unmarshal([]byte(raw), &limits); err != nil {
			return fmt.Errorf("invalid model_request_limits: %w", err)
		}
	}
	return runtimebudget.ModelGate().Configure(limits)
}

// Persist the pair atomically before publishing the runtime configuration.
// The write lock prevents older concurrent saves from being applied last.
func (m *Manager) setModelRequestLimits(ctx context.Context, limits runtimebudget.Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.modelLimitsMu.Lock()
	defer m.modelLimitsMu.Unlock()
	raw, _ := json.Marshal(limits)
	if err := m.pg.SetSettingContext(ctx, settingModelRequestLimits, string(raw)); err != nil {
		return err
	}
	return runtimebudget.ModelGate().Configure(limits)
}

func (m *Manager) loadModelRequestDeadlines() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, found, err := m.pg.GetSettingContext(ctx, settingModelRequestDeadlines)
	if err != nil {
		return err
	}
	deadlines := runtimebudget.DefaultModelDeadlines()
	if found {
		if err = json.Unmarshal([]byte(raw), &deadlines); err != nil {
			return fmt.Errorf("invalid model_request_deadlines: %w", err)
		}
	}
	return runtimebudget.ConfigureModelDeadlines(deadlines)
}

func (m *Manager) setModelRequestDeadlines(ctx context.Context, deadlines runtimebudget.ModelDeadlines) error {
	if err := deadlines.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.modelLimitsMu.Lock()
	defer m.modelLimitsMu.Unlock()
	raw, _ := json.Marshal(deadlines)
	if err := m.pg.SetSettingContext(ctx, settingModelRequestDeadlines, string(raw)); err != nil {
		return err
	}
	return runtimebudget.ConfigureModelDeadlines(deadlines)
}

func (m *Manager) loadModelByteLimits() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, found, err := m.pg.GetSettingContext(ctx, settingModelByteLimits)
	if err != nil {
		return err
	}
	limits := runtimebudget.DefaultModelByteLimits()
	if found {
		if err = json.Unmarshal([]byte(raw), &limits); err != nil {
			return fmt.Errorf("invalid model_request_bytes: %w", err)
		}
	}
	return runtimebudget.ConfigureModelByteLimits(limits)
}

func (m *Manager) setModelByteLimits(ctx context.Context, limits runtimebudget.ModelByteLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.modelLimitsMu.Lock()
	defer m.modelLimitsMu.Unlock()
	raw, _ := json.Marshal(limits)
	if err := m.pg.SetSettingContext(ctx, settingModelByteLimits, string(raw)); err != nil {
		return err
	}
	return runtimebudget.ConfigureModelByteLimits(limits)
}

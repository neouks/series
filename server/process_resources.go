package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/neouks/series/agent"
	"github.com/neouks/series/runtimebudget"
)

const settingExternalProcessLimits = "external_process_limits"

func externalProcessKey(ctx context.Context) string {
	return agent.ExternalProcessKey(ctx)
}

func (m *Manager) loadExternalProcessLimits() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, found, err := m.pg.GetSettingContext(ctx, settingExternalProcessLimits)
	if err != nil {
		return err
	}
	limits := runtimebudget.Limits{Concurrency: 32, Queue: 256}
	if found {
		if err = json.Unmarshal([]byte(raw), &limits); err != nil {
			return fmt.Errorf("invalid external_process_limits: %w", err)
		}
	}
	return runtimebudget.ProcessGate().Configure(limits)
}
func (m *Manager) setExternalProcessLimits(ctx context.Context, limits runtimebudget.Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.modelLimitsMu.Lock()
	defer m.modelLimitsMu.Unlock()
	raw, _ := json.Marshal(limits)
	if err := m.pg.SetSettingContext(ctx, settingExternalProcessLimits, string(raw)); err != nil {
		return err
	}
	return runtimebudget.ProcessGate().Configure(limits)
}

// The SDK owns stdio child creation/reaping. Its Close waits for the real child;
// keep this permit until Close returns, including failed handshakes and tests.
type admittedMCPClient struct {
	mcpClient
	once    sync.Once
	release func()
	err     error
}

func (c *admittedMCPClient) Close() error {
	c.once.Do(func() { c.err = c.mcpClient.Close(); c.release() })
	return c.err
}

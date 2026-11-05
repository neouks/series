package agent

import (
	"context"
	"errors"
	"fmt"
)

var errSessionRecovery = errors.New("会话历史恢复失败，本轮已停止，未按空会话重新执行")

// Missing histories are fresh sessions in the official SDK. Every other load
// error must stop before Prompt: silently continuing could repeat completed
// actions while appending an unrelated run to the same audit file.
func restoreConversation(s conversationSession, id string) error {
	return restoreConversationContext(context.Background(), s, id)
}

func restoreConversationContext(ctx context.Context, s conversationSession, id string) error {
	var err error
	if contextual, ok := s.(interface {
		ResumeContext(context.Context, string) error
	}); ok {
		err = contextual.ResumeContext(ctx, id)
	} else {
		err = s.Resume(id)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errSessionRecovery, err)
	}
	return nil
}

func sessionHasWorkerRequest(ctx context.Context, s conversationSession, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if recovered, ok := s.(interface {
		HasWorkerRequest(context.Context, string) (bool, error)
	}); ok {
		found, err := recovered.HasWorkerRequest(ctx, id)
		if err != nil {
			return false, fmt.Errorf("%w: delivery metadata: %w", errSessionRecovery, err)
		}
		return found, nil
	}
	return hasWorkerChatMessage(s.Messages(), id), nil
}

func seedSessionUnlock(ctx context.Context, s conversationSession, unlock func(string)) error {
	if unlock == nil {
		return nil
	}
	seen := map[string]bool{}
	apply := func(name string) {
		if !seen[name] {
			seen[name] = true
			unlock(name)
		}
	}
	if recovered, ok := s.(interface {
		ReplayRecoveredSkills(context.Context, func(string)) error
	}); ok {
		if err := recovered.ReplayRecoveredSkills(ctx, apply); err != nil {
			return fmt.Errorf("%w: skill metadata: %w", errSessionRecovery, err)
		}
	}
	seedUnlockFromHistory(s.Messages(), apply)
	return nil
}

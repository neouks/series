package agent

import (
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestWorkerSessionIDIsStablePerIntent(t *testing.T) {
	if got := WorkerSessionID(12, 34); got != "exp12-worker-i34" {
		t.Fatalf("session id = %q, want exp12-worker-i34", got)
	}
}

func TestWorkerChatMarkerOnlyMatchesItsNormalUserTurn(t *testing.T) {
	requestID := "worker-message-123"
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(workerChatMarker(requestID))}},
		llm.UserText(workerChatMarker("worker-message-other") + "\nother"),
		llm.UserText(workerChatMarker(requestID) + "\nnew intent"),
	}
	if !hasWorkerChatMessage(messages, requestID) {
		t.Fatal("expected the matching user turn to be detected")
	}
	if hasWorkerChatMessage(messages, "worker-message-missing") {
		t.Fatal("unrelated request id matched a Worker user turn")
	}
}

func TestWorkerChatDeliveryIdentitySurvivesBrandChanges(t *testing.T) {
	for _, tc := range []struct {
		text, id string
		want     bool
	}{
		{"<!-- PREVIOUS_WORKER_CHAT:request-1 -->", "request-1", true},
		{"<!-- SERIES_WORKER_CHAT:request-1 -->", "request-1", true},
		{"<!-- PREVIOUS_WORKER_CHAT:request-10 -->", "request-1", false},
		{"PREVIOUS_WORKER_CHAT:request-1", "request-1", false},
		{"<!-- PREVIOUS_WORKER_CHAT:request-1", "request-1", false},
		{"<!-- OTHER:request-1 -->", "request-1", false},
		{"<!-- PREVIOUS WORKER_CHAT:request-1 -->", "request-1", false},
		{"<!-- SERIES_WORKER_CHAT: -->", "", false},
	} {
		messages := []llm.Message{llm.UserText(tc.text)}
		if got := hasWorkerChatMessage(messages, tc.id); got != tc.want {
			t.Errorf("message=%q id=%q: got %v want %v", tc.text, tc.id, got, tc.want)
		}
		if messages[0].Text() != tc.text {
			t.Fatal("original transcript changed")
		}
	}
}
